//go:build integration

// orchestrator_integration_test.go drives the full retry pipeline
// against real Postgres 16 + real Redis 7.4 containers:
//
//   - PgLeader takes the advisory lock.
//   - PgReader reads mature respondent rows (FOR UPDATE SKIP LOCKED).
//   - The orchestrator decrypts via a fake Decryptor (no KMS in the
//     integration build), enqueues into a real RedisQueue, and marks
//     the row scheduled in the respondents table.
//
// Two-instance leader-election test: instance A leads, instance B
// observes leader_active=0; A cancels → B takes the lock on the next
// tick. Mirrors the production failover invariant.
//
// Build tag `integration` keeps the testcontainer overhead out of the
// default test run.
package retry_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/queue"
	"github.com/sociopulse/platform/internal/dialer/retry"
	"github.com/sociopulse/platform/pkg/postgres"
)

// startRedis boots Redis 7.4 in a container and returns a connected
// *redis.Client. Cleanup is registered via t.Cleanup.
func startRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req := testcontainers.ContainerRequest{
		Image:        "redis:7.4-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)
	rdb := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// passthroughDecryptor returns the ciphertext bytes verbatim. The
// real KMS path lives in tenancy.KMSResolver; this integration test
// exercises the orchestrator's pipeline (PG read → decrypt → queue →
// PG update) without standing up the full encryption stack.
type passthroughDecryptor struct{}

func (passthroughDecryptor) Decrypt(_ context.Context, _ uuid.UUID, ciphertext []byte) ([]byte, error) {
	return ciphertext, nil
}

// seedRespondent inserts a tenant + project + respondent row. The
// tenant is created via BypassRLS (platform-internal); project +
// respondent live under WithTenant. Returns (tenantID, projectID,
// respondentID).
//
// The respondent's next_attempt_at is back-dated by 1 minute so it's
// mature on every sweep tick.
func seedRespondent(t *testing.T, pool *postgres.Pool, attempts int) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenantID := uuid.New()
	projectID := uuid.New()
	respondentID := uuid.New()
	phoneCT := []byte("+79991234567")
	phoneHash := []byte("hash-" + respondentID.String()[:16])

	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			VALUES ($1, $2, 'Test', 'active', 'kek-test', '\x00010203')
		`, tenantID, "test-org-"+tenantID.String()[:8])
		return err
	}))

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO projects (id, tenant_id, code, name, status)
			VALUES ($1, $2, $3, 'Project A', 'active')
		`, projectID, tenantID, "proj-"+projectID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO respondents
				(id, tenant_id, project_id, phone_encrypted, phone_hash, region_code,
				 status, attempts, next_attempt_at, source)
			VALUES ($1, $2, $3, $4, $5, 'RU-MOW',
			        'pending', $6, now() - interval '1 minute', 'imported')
		`, respondentID, tenantID, projectID, phoneCT, phoneHash, attempts)
		return err
	}))
	return tenantID, projectID, respondentID
}

// readRespondentStatus returns the current respondents.status for id.
func readRespondentStatus(t *testing.T, pool *postgres.Pool, id uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	var status string
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status FROM respondents WHERE id = $1`, id).Scan(&status)
	}))
	return status
}

// TestIntegration_Orchestrator_EnqueuesMatureRow — happy-path: a
// pending respondent with next_attempt_at <= now() is picked up,
// enqueued, and marked dialing.
func TestIntegration_Orchestrator_EnqueuesMatureRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)
	rdb := startRedis(t)
	logger := zaptest.NewLogger(t)

	tenantID, projectID, respondentID := seedRespondent(t, pool, 1)

	// Real RedisQueue.
	q, err := queue.New(queue.Config{
		Redis:   rdb,
		Logger:  logger.Named("queue"),
		Metrics: queue.RegisterMetrics(prometheus.NewRegistry()),
	})
	require.NoError(t, err)

	leader, err := retry.NewPgLeader(pool, retry.DefaultLockKey, logger.Named("leader"))
	require.NoError(t, err)
	t.Cleanup(func() { leader.Release(context.Background()) })
	reader, err := retry.NewPgReader(pool)
	require.NoError(t, err)

	o, err := retry.New(retry.Config{
		Leader:      leader,
		Reader:      reader,
		Decryptor:   passthroughDecryptor{},
		Queue:       q,
		Interval:    100 * time.Millisecond,
		BatchLimit:  10,
		MaxAttempts: 3,
		Logger:      logger.Named("retry"),
		Metrics:     retry.RegisterMetrics(prometheus.NewRegistry()),
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()

	// Wait for the row to be marked 'dialing' (i.e. sweep has run).
	require.Eventually(t, func() bool {
		return readRespondentStatus(t, pool, respondentID) == "dialing"
	}, 5*time.Second, 50*time.Millisecond, "respondent must transition to 'dialing' after sweep")

	// Queue size for that project should be 1.
	size, err := q.Size(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, int64(1), size)

	// Pop the queue item — must be our respondent with priority and
	// AttemptN derived from the seed.
	item, err := q.PickNext(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, respondentID, item.RespondentID)
	require.Equal(t, "+79991234567", item.Phone)
	require.Equal(t, "RU-MOW", item.Region)
	require.Equal(t, uint8(2), item.AttemptN, "AttemptN = row.attempts(1) + 1")
	require.Equal(t, uint8(2), item.Priority, "Priority = min(1+attempts, 9) = 2")

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// TestIntegration_Orchestrator_ExhaustsAtCap — a row at the cap is
// marked exhausted, never enqueued.
func TestIntegration_Orchestrator_ExhaustsAtCap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)
	rdb := startRedis(t)
	logger := zaptest.NewLogger(t)

	_, _, respondentID := seedRespondent(t, pool, 3) // attempts=maxAttempts

	q, err := queue.New(queue.Config{
		Redis:   rdb,
		Logger:  logger.Named("queue"),
		Metrics: queue.RegisterMetrics(prometheus.NewRegistry()),
	})
	require.NoError(t, err)

	leader, err := retry.NewPgLeader(pool, retry.DefaultLockKey, logger.Named("leader"))
	require.NoError(t, err)
	t.Cleanup(func() { leader.Release(context.Background()) })
	reader, err := retry.NewPgReader(pool)
	require.NoError(t, err)

	o, err := retry.New(retry.Config{
		Leader:      leader,
		Reader:      reader,
		Decryptor:   passthroughDecryptor{},
		Queue:       q,
		Interval:    100 * time.Millisecond,
		BatchLimit:  10,
		MaxAttempts: 3,
		Logger:      logger.Named("retry"),
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()

	require.Eventually(t, func() bool {
		return readRespondentStatus(t, pool, respondentID) == "exhausted"
	}, 5*time.Second, 50*time.Millisecond)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// TestIntegration_Orchestrator_TwoInstancesElectLeader — two
// orchestrators contend on the lock; only one drives the sweep. After
// the leader's ctx cancels, the peer takes over on the next tick and
// processes a freshly-seeded row.
func TestIntegration_Orchestrator_TwoInstancesElectLeader(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)
	rdb := startRedis(t)
	logger := zaptest.NewLogger(t)

	const lockKey int64 = 0x4eed_face_dead_beef

	q, err := queue.New(queue.Config{
		Redis:   rdb,
		Logger:  logger.Named("queue"),
		Metrics: queue.RegisterMetrics(prometheus.NewRegistry()),
	})
	require.NoError(t, err)

	// Build TWO orchestrators contending on the same lockKey, each
	// with its own metrics registry so we can compare the
	// leader_active gauges.
	mkOrchestrator := func(name string) (*retry.Orchestrator, *retry.Metrics, *retry.PgLeader) {
		leader, err := retry.NewPgLeader(pool, lockKey, logger.Named(name+".leader"))
		require.NoError(t, err)
		reader, err := retry.NewPgReader(pool)
		require.NoError(t, err)
		metrics := retry.RegisterMetrics(prometheus.NewRegistry())
		o, err := retry.New(retry.Config{
			Leader:      leader,
			Reader:      reader,
			Decryptor:   passthroughDecryptor{},
			Queue:       q,
			Interval:    100 * time.Millisecond,
			BatchLimit:  10,
			MaxAttempts: 3,
			LockKey:     lockKey,
			Logger:      logger.Named(name),
			Metrics:     metrics,
		})
		require.NoError(t, err)
		return o, metrics, leader
	}

	oA, mA, lA := mkOrchestrator("a")
	oB, mB, lB := mkOrchestrator("b")
	t.Cleanup(func() {
		lA.Release(context.Background())
		lB.Release(context.Background())
	})

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	var wg sync.WaitGroup
	// Go 1.25 wg.Go — replaces wg.Add(1); go func(){ defer wg.Done(); ... }()
	// per Plan 09 carry-forward checklist #11.
	wg.Go(func() {
		doneA <- oA.Run(ctxA)
	})
	wg.Go(func() {
		doneB <- oB.Run(ctxB)
	})

	// Wait for exactly ONE instance to report leader_active=1.
	require.Eventually(t, func() bool {
		a := testutil.ToFloat64(mA.LeaderActive)
		b := testutil.ToFloat64(mB.LeaderActive)
		return (a == 1 && b == 0) || (a == 0 && b == 1)
	}, 5*time.Second, 50*time.Millisecond, "exactly one instance must lead")

	// Identify the leader via IsLeading on the underlying PgLeader.
	// Track everything by an "aLeads" boolean — Go forbids comparing
	// CancelFunc values with ==, so the follower side uses the inverse.
	aLeads := lA.IsLeading()
	if !aLeads {
		require.True(t, lB.IsLeading(), "exactly one PgLeader must lead")
	}

	// Seed a row that the FOLLOWER will process after takeover.
	_, _, respondentID := seedRespondent(t, pool, 0)

	// Kill the leader. Its Release on shutdown frees the advisory lock
	// so the follower can take over on the next tick.
	var (
		leaderDone, followerDone <-chan error
		followerLeader           *retry.PgLeader
		followerMetrics          *retry.Metrics
		followerCancel           context.CancelFunc
	)
	if aLeads {
		cancelA()
		leaderDone, followerDone = doneA, doneB
		followerLeader, followerMetrics, followerCancel = lB, mB, cancelB
	} else {
		cancelB()
		leaderDone, followerDone = doneB, doneA
		followerLeader, followerMetrics, followerCancel = lA, mA, cancelA
	}
	require.ErrorIs(t, <-leaderDone, context.Canceled)

	// Follower acquires on its next tick and processes the row.
	require.Eventually(t, func() bool {
		return followerLeader.IsLeading()
	}, 5*time.Second, 50*time.Millisecond, "follower must take over after leader exits")

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(followerMetrics.LeaderActive) == 1
	}, 5*time.Second, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		return readRespondentStatus(t, pool, respondentID) == "dialing"
	}, 5*time.Second, 50*time.Millisecond, "follower must process the freshly-seeded row")

	// Cancel the follower; the test cleans up.
	followerCancel()
	require.ErrorIs(t, <-followerDone, context.Canceled)
	wg.Wait()
}

// TestIntegration_PgReader_ListMatureRetriesClampsLimit — out-of-band
// limits clamp to the documented bracket; an empty database is a
// no-op (nil rows, nil error). Exercises the clampLimit branches not
// reachable via the orchestrator's defaulted-100 batch size.
func TestIntegration_PgReader_ListMatureRetriesClampsLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)
	reader, err := retry.NewPgReader(pool)
	require.NoError(t, err)
	ctx := context.Background()

	// Limit < 1 clamps to 1 (no-op against an empty DB).
	rows, err := reader.ListMatureRetries(ctx, 0)
	require.NoError(t, err)
	require.Empty(t, rows)

	rows, err = reader.ListMatureRetries(ctx, -100)
	require.NoError(t, err)
	require.Empty(t, rows)

	// Limit > 1000 clamps to 1000 (no-op against an empty DB).
	rows, err = reader.ListMatureRetries(ctx, 5000)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestIntegration_Orchestrator_SkipsForUpdateLocked — a row already
// locked by a transaction in another session must be skipped (FOR
// UPDATE SKIP LOCKED). The orchestrator should still process the
// other mature rows in the same sweep.
func TestIntegration_Orchestrator_SkipsForUpdateLocked(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPG(t)
	rdb := startRedis(t)
	logger := zaptest.NewLogger(t)

	tenantID, _, lockedRespondentID := seedRespondent(t, pool, 0)

	// Seed a SECOND respondent in the same project — both should be
	// mature, but the first is row-locked while the orchestrator runs.
	otherRespondentID := uuid.New()
	require.NoError(t, pool.WithTenant(context.Background(), tenantID, func(tx postgres.Tx) error {
		// Find any project for this tenant to attach the second
		// respondent to. The seed already created one.
		var projectID uuid.UUID
		require.NoError(t, tx.QueryRow(context.Background(),
			`SELECT id FROM projects WHERE tenant_id = $1 LIMIT 1`, tenantID).Scan(&projectID))
		_, err := tx.Exec(context.Background(), `
			INSERT INTO respondents
				(id, tenant_id, project_id, phone_encrypted, phone_hash, region_code,
				 status, attempts, next_attempt_at, source)
			VALUES ($1, $2, $3, $4, $5, 'RU-MOW',
			        'pending', 0, now() - interval '1 minute', 'imported')
		`, otherRespondentID, tenantID, projectID, []byte("+79990000001"),
			[]byte("hash-other-"+otherRespondentID.String()[:8]))
		return err
	}))

	// Hold a row-lock on the first respondent in a separate
	// long-running tx; the orchestrator's SKIP LOCKED must omit it.
	holdCtx, holdCancel := context.WithCancel(context.Background())
	defer holdCancel()
	holdDone := make(chan struct{})
	go func() {
		defer close(holdDone)
		_ = pool.BypassRLS(holdCtx, func(tx postgres.Tx) error {
			var dummy uuid.UUID
			if err := tx.QueryRow(holdCtx,
				`SELECT id FROM respondents WHERE id = $1 FOR UPDATE`,
				lockedRespondentID).Scan(&dummy); err != nil {
				return err
			}
			// Hold the row until ctx cancels.
			<-holdCtx.Done()
			return nil
		})
	}()

	// Give the lock-holder time to grab the row.
	time.Sleep(200 * time.Millisecond)

	q, err := queue.New(queue.Config{
		Redis:   rdb,
		Logger:  logger.Named("queue"),
		Metrics: queue.RegisterMetrics(prometheus.NewRegistry()),
	})
	require.NoError(t, err)
	leader, err := retry.NewPgLeader(pool, retry.DefaultLockKey, logger.Named("leader"))
	require.NoError(t, err)
	t.Cleanup(func() { leader.Release(context.Background()) })
	reader, err := retry.NewPgReader(pool)
	require.NoError(t, err)

	o, err := retry.New(retry.Config{
		Leader:      leader,
		Reader:      reader,
		Decryptor:   passthroughDecryptor{},
		Queue:       q,
		Interval:    100 * time.Millisecond,
		BatchLimit:  10,
		MaxAttempts: 3,
		Logger:      logger.Named("retry"),
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()

	// The OTHER row gets processed.
	require.Eventually(t, func() bool {
		return readRespondentStatus(t, pool, otherRespondentID) == "dialing"
	}, 5*time.Second, 50*time.Millisecond)

	// The locked row stays pending (SKIP LOCKED omitted it).
	require.Equal(t, "pending", readRespondentStatus(t, pool, lockedRespondentID))

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	// Release the lock-holder so the test cleans up.
	holdCancel()
	<-holdDone
}

// Compile-time interface confirmations — these surface drift between
// the api surface and the production wiring at integration-build time.
var (
	_ retry.Decryptor        = passthroughDecryptor{}
	_ retry.Leader           = (*retry.PgLeader)(nil)
	_ retry.RespondentReader = (*retry.PgReader)(nil)
	_ api.RetryOrchestrator  = (*retry.Orchestrator)(nil)
)

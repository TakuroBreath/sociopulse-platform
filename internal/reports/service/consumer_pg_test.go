//go:build integration

// Integration tests for service.Consumer against a real Postgres
// testcontainer + LocalObjectStore. Sister to internal/reports/store/
// pg_pg_test.go (which covers the bare *Tx state-flip variants); this
// file exercises the full Consumer.handleJobRun pipeline end-to-end.
//
// The asynq.Server lifecycle is NOT exercised here — that would require
// a Redis-backed testcontainer or miniredis, and the unit tests in
// consumer_test.go already cover Run / Shutdown via the AsynqServer
// fake. handleJobRun is the orthogonal compute path: it runs the
// state-flip + render + upload + audit + outbox publish atomically
// against the real DB.

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap/zaptest"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	storage "github.com/sociopulse/platform/internal/recording/storage"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	rptevents "github.com/sociopulse/platform/internal/reports/events"
	reportsvc "github.com/sociopulse/platform/internal/reports/service"
	reportstore "github.com/sociopulse/platform/internal/reports/store"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// startReportsServicePG boots Postgres 16, applies every migration up
// through 000012, and returns a connected *postgres.Pool. Mirrors the
// pattern in internal/reports/store/pg_pg_test.go.
func startReportsServicePG(t *testing.T) *postgres.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("sociopulse_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	migrationsURL := reportsServiceMigrationsURL(t)
	mig, err := migrate.New(migrationsURL, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = mig.Close() })
	require.NoError(t, mig.Up())

	pool, err := postgres.Open(ctx, postgres.Config{
		DSN:            dsn,
		MaxConns:       8,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func reportsServiceMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err, "migrations dir not found at %s", abs)
	return "file://" + abs
}

// seedReportsServiceTenant inserts a fresh tenant row (BypassRLS) and
// returns its ID.
func seedReportsServiceTenant(t *testing.T, pool *postgres.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			 VALUES ($1, $2, $3, 'active', 'kms-test', '\x00')`,
			id,
			"org-"+id.String(),
			"tenant-"+id.String(),
		)
		return err
	}))
	return id
}

// buildIntegrationConsumer wires a Consumer against the real pool +
// LocalObjectStore + outbox.PostgresWriter, with the supplied analytics
// fake. The Mark*Tx flip closures are then re-routed to a fixed jobID
// via SetConsumerFlipsForTest — production reads the jobID from
// asynq.GetTaskID(ctx), which yields "" outside the real asynq.Server,
// so tests use the wrapping trick to thread the real jobID through.
func buildIntegrationConsumer(
	t *testing.T, pool *postgres.Pool, analytics analyticsapi.ServiceRO, jobID string,
) *reportsvc.Consumer {
	t.Helper()
	objStore := storage.NewLocalObjectStore()
	pw := outbox.NewPostgresWriter()

	c := reportsvc.NewConsumer(reportsvc.ConsumerDeps{
		Server:       nil,
		Analytics:    analytics,
		Pool:         pool,
		ObjectStore:  objStore,
		Audit:        reportsvc.NewAuditEmitter(pw),
		ReadyPub:     rptevents.NewReportReadyPublisher(pw),
		BucketPrefix: "sociopulse-reports",
		PresignTTL:   24 * time.Hour,
		Logger:       zaptest.NewLogger(t),
		Now:          func() time.Time { return time.Now().UTC() },
	})

	// Force the flip closures to use the test-known jobID — production's
	// jobID comes from asynq.GetTaskID(ctx) but our ctx in tests is
	// stdlib context.Context, so GetTaskID returns "". We wrap the real
	// Mark*Tx free functions to swap in our jobID.
	wrapRunning := func(ctx context.Context, tx postgres.Tx, _ string, ts time.Time) error {
		return reportstore.MarkRunningTx(ctx, tx, jobID, ts)
	}
	wrapSucceeded := func(ctx context.Context, tx postgres.Tx, _ string, ts time.Time, bytesSize int64, filename, url string) error {
		return reportstore.MarkSucceededTx(ctx, tx, jobID, ts, bytesSize, filename, url)
	}
	wrapFailed := func(ctx context.Context, tx postgres.Tx, _ string, ts time.Time, errMsg string) error {
		return reportstore.MarkFailedTx(ctx, tx, jobID, ts, errMsg)
	}
	reportsvc.SetConsumerFlipsForTest(c, wrapRunning, wrapSucceeded, wrapFailed)

	return c
}

// seedQueuedJob inserts a fresh reports_jobs row in 'queued' state and
// returns the jobID + JobInput.
func seedQueuedJob(t *testing.T, st *reportstore.PG, tenantID uuid.UUID,
	kind reportsapi.ReportKind, format reportsapi.ExportFormat, params map[string]any,
) (string, reportsapi.JobInput) {
	t.Helper()
	in := reportsapi.JobInput{
		RenderInput: reportsapi.RenderInput{
			Kind:     kind,
			Format:   format,
			Params:   params,
			Window:   analyticsapi.Window{From: dataFrom, To: dataTo},
			TenantID: tenantID,
			ActorID:  uuid.New(),
		},
		NotifyUserID: uuid.New(),
	}
	jobID := uuid.Must(uuid.NewV7()).String()
	require.NoError(t, st.Create(t.Context(), reportstore.CreateInput{
		ID:           jobID,
		TenantID:     tenantID,
		Kind:         in.Kind,
		Format:       in.Format,
		Params:       in.Params,
		WindowFrom:   in.Window.From,
		WindowTo:     in.Window.To,
		CreatedBy:    in.ActorID,
		NotifyUserID: in.NotifyUserID,
	}))
	return jobID, in
}

// countOutboxBySubject reads the number of outbox rows for a given
// tenant + subject. BypassRLS because event_outbox has no RLS.
func countOutboxBySubject(t *testing.T, pool *postgres.Pool, tenantID uuid.UUID, subject string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		return tx.QueryRow(t.Context(),
			`SELECT count(*) FROM event_outbox WHERE tenant_id = $1 AND subject = $2`,
			tenantID, subject,
		).Scan(&n)
	}))
	return n
}

// ─── Integration tests ───────────────────────────────────────────────────────

// TestConsumerIntegration_HappyPath verifies the complete async pipeline
// end-to-end: handleJobRun flips the row queued→running→succeeded,
// uploads the artifact, signs a presigned URL, writes the audit-event
// and report-ready outbox rows in one tx.
func TestConsumerIntegration_HappyPath(t *testing.T) {
	t.Parallel()
	pool := startReportsServicePG(t)
	st := reportstore.NewPG(pool)

	tenantID := seedReportsServiceTenant(t, pool)
	analytics := &fakeAnalytics{
		comparisons: []analyticsapi.OperatorComparisonRow{{
			OperatorID:  uuid.New(),
			DisplayName: "X",
		}},
	}
	jobID, in := seedQueuedJob(t, st, tenantID,
		reportsapi.KindOperatorEfficiency, reportsapi.FormatXLSX,
		map[string]any{"project_id": uuid.New().String()})

	c := buildIntegrationConsumer(t, pool, analytics, jobID)

	payload, err := json.Marshal(in)
	require.NoError(t, err)
	task := asynq.NewTask(reportsapi.TaskJobRun, payload)

	require.NoError(t, reportsvc.HandleJobRunForTest(c, t.Context(), task))

	// Row must be in 'succeeded'.
	got, err := st.Get(t.Context(), tenantID, jobID)
	require.NoError(t, err)
	require.Equal(t, reportsapi.JobSucceeded, got.State)
	require.Positive(t, got.BytesSize)
	require.NotEmpty(t, got.Filename)
	require.NotEmpty(t, got.DownloadURL, "presigned URL must be persisted on succeed")

	// Both outbox events committed in the same tx.
	require.Equal(t, 1,
		countOutboxBySubject(t, pool, tenantID, "tenant."+tenantID.String()+".audit.event"))
	require.Equal(t, 1,
		countOutboxBySubject(t, pool, tenantID, "tenant."+tenantID.String()+".reports.report.ready"))
}

// TestConsumerIntegration_PermanentFailure verifies that a render error
// classified as permanent (ErrInvalidParams) flips the row to 'failed',
// emits an audit-event with the error context, and returns a
// SkipRetry-wrapped error so asynq archives the task instead of
// retrying. No report-ready event on failure.
func TestConsumerIntegration_PermanentFailure(t *testing.T) {
	t.Parallel()
	pool := startReportsServicePG(t)
	st := reportstore.NewPG(pool)

	tenantID := seedReportsServiceTenant(t, pool)
	// FetchOperatorEfficiency requires project_id; empty params → ErrInvalidParams.
	jobID, in := seedQueuedJob(t, st, tenantID,
		reportsapi.KindOperatorEfficiency, reportsapi.FormatXLSX,
		map[string]any{})

	c := buildIntegrationConsumer(t, pool, &fakeAnalytics{}, jobID)

	payload, err := json.Marshal(in)
	require.NoError(t, err)
	task := asynq.NewTask(reportsapi.TaskJobRun, payload)

	err = reportsvc.HandleJobRunForTest(c, t.Context(), task)
	require.Error(t, err)
	require.ErrorIs(t, err, asynq.SkipRetry, "permanent failure must wrap SkipRetry")

	got, err := st.Get(t.Context(), tenantID, jobID)
	require.NoError(t, err)
	require.Equal(t, reportsapi.JobFailed, got.State)
	require.NotEmpty(t, got.Error, "error message must be persisted")

	require.Equal(t, 1,
		countOutboxBySubject(t, pool, tenantID, "tenant."+tenantID.String()+".audit.event"),
		"exactly one audit event for the failed export")
	require.Equal(t, 0,
		countOutboxBySubject(t, pool, tenantID, "tenant."+tenantID.String()+".reports.report.ready"),
		"no report-ready event when the export failed")
}

// TestConsumerIntegration_StaleSkipAcks verifies that a row already in
// a terminal state (pre-canceled) does NOT get re-flipped — the
// MarkRunning CAS predicate returns RowsAffected=0 → ErrStaleSkip →
// handler acks without further work.
func TestConsumerIntegration_StaleSkipAcks(t *testing.T) {
	t.Parallel()
	pool := startReportsServicePG(t)
	st := reportstore.NewPG(pool)

	tenantID := seedReportsServiceTenant(t, pool)
	jobID, in := seedQueuedJob(t, st, tenantID,
		reportsapi.KindOperatorEfficiency, reportsapi.FormatXLSX,
		map[string]any{"project_id": uuid.New().String()})

	// Pre-cancel the row.
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return reportstore.MarkCanceledTx(t.Context(), tx, jobID, time.Now().UTC())
	}))

	c := buildIntegrationConsumer(t, pool, &fakeAnalytics{}, jobID)

	payload, err := json.Marshal(in)
	require.NoError(t, err)
	task := asynq.NewTask(reportsapi.TaskJobRun, payload)

	require.NoError(t, reportsvc.HandleJobRunForTest(c, t.Context(), task),
		"already-terminal row must ack without further work")

	got, err := st.Get(t.Context(), tenantID, jobID)
	require.NoError(t, err)
	require.Equal(t, reportsapi.JobCanceled, got.State, "Consumer did not regress the row")

	// No outbox writes — Consumer short-circuited.
	var auditCount int
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		return tx.QueryRow(t.Context(),
			`SELECT count(*) FROM event_outbox WHERE tenant_id = $1`, tenantID).Scan(&auditCount)
	}))
	require.Zero(t, auditCount, "no outbox writes when MarkRunningTx stale-skips")
}

// TestConsumerIntegration_QueueAndCancel verifies the Queue.Enqueue →
// store.Create round-trip against the real DB, plus Queue.Cancel
// idempotency on a terminal row. Mirrors the unit-test happy path
// but with a real Postgres tx for the MarkCanceledTx state flip.
func TestConsumerIntegration_QueueAndCancel(t *testing.T) {
	t.Parallel()
	pool := startReportsServicePG(t)
	st := reportstore.NewPG(pool)

	tenantID := seedReportsServiceTenant(t, pool)
	taskID := uuid.Must(uuid.NewV7()).String()
	enq := &fakeIntegrationEnqueuer{nextID: taskID}
	q := reportsvc.NewQueue(st, pool, enq, "reports")

	in := reportsapi.JobInput{
		RenderInput: reportsapi.RenderInput{
			Kind:     reportsapi.KindCallsByStatus,
			Format:   reportsapi.FormatCSV,
			Params:   map[string]any{},
			Window:   analyticsapi.Window{From: dataFrom, To: dataTo},
			TenantID: tenantID,
			ActorID:  uuid.New(),
		},
		NotifyUserID: uuid.New(),
	}
	ticket, err := q.Enqueue(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, taskID, ticket.JobID)

	got, err := q.Get(t.Context(), ticket.JobID)
	require.NoError(t, err)
	require.Equal(t, reportsapi.JobQueued, got.State)

	// Cancel the queued job — state flips to canceled.
	require.NoError(t, q.Cancel(t.Context(), ticket.JobID))
	got2, err := q.Get(t.Context(), ticket.JobID)
	require.NoError(t, err)
	require.Equal(t, reportsapi.JobCanceled, got2.State)

	// Second cancel must be idempotent (ErrStaleSkip swallowed by Queue.Cancel).
	require.NoError(t, q.Cancel(t.Context(), ticket.JobID))
}

// fakeIntegrationEnqueuer is the integration-test asynq enqueuer — no
// Redis touching, just returns a pre-configured TaskInfo.
type fakeIntegrationEnqueuer struct {
	nextID string
}

func (e *fakeIntegrationEnqueuer) EnqueueContext(_ context.Context, task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	if e.nextID == "" {
		return nil, errors.New("fakeIntegrationEnqueuer: nextID empty")
	}
	return &asynq.TaskInfo{ID: e.nextID, Type: task.Type(), Payload: task.Payload(), Queue: "reports"}, nil
}

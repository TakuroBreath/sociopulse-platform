//go:build integration

// audit_pg_test.go drives the Machine against real Postgres 16 +
// real Redis 7.4 containers and asserts the canonical end-to-end audit
// shape:
//
//   - StartShift inserts exactly one operator_sessions row + one
//     operator_state_log row + one event_outbox row, all in the same Tx.
//   - Subsequent transitions append one (state_log, outbox) pair each.
//   - EndShift sets ended_at on the operator_sessions row.
//
// Migrations are run via golang-migrate (file:// from the repo's
// migrations/ dir).
package fsm_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/fsm"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// startPGContainer boots Postgres 16 in a container, applies all
// project migrations, and returns a connected *postgres.Pool.
func startPGContainer(t *testing.T) *postgres.Pool {
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

	migrationsURL := repoMigrationsURL(t)
	mig, err := migrate.New(migrationsURL, dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = mig.Close()
	})
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

// repoMigrationsURL returns the file:// URL of the repo's migrations
// dir, resolved relative to this test file's location.
func repoMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err)
	return "file://" + abs
}

// seedPrereqRows inserts a tenants row (BypassRLS — tenants is a
// platform-internal table), a users row, and a projects row (both
// inside per-tenant tx via WithTenant — RLS-protected). Returns
// (tenantID, userID, projectID).
func seedPrereqRows(t *testing.T, pool *postgres.Pool) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenantID, userID, projectID := uuid.New(), uuid.New(), uuid.New()

	// tenants — owned by tenancy_admin; insert via BypassRLS.
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			VALUES ($1, $2, 'Test', 'active', 'kek-test', '\x00010203')
		`, tenantID, "test-org-"+tenantID.String()[:8])
		return err
	}))

	// users + projects — RLS-protected; insert via WithTenant.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, tenant_id, login, password_hash, full_name, roles)
			VALUES ($1, $2, $3, 'argon2id-stub', 'Op', ARRAY['operator'])
		`, userID, tenantID, "op-"+userID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO projects (id, tenant_id, code, name, status)
			VALUES ($1, $2, $3, 'Project A', 'active')
		`, projectID, tenantID, "proj-"+projectID.String()[:8]); err != nil {
			return err
		}
		return nil
	}))
	return tenantID, userID, projectID
}

// startRedisOnly is a smaller helper than startRedis() in the redis
// test file — kept here to avoid a circular dependency between build
// tags. testcontainers manages its own per-test container lifecycle so
// running both integration tests in one binary is fine.
func startRedisOnly(t *testing.T) *redis.Client {
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

// TestIntegration_PG_FullAuditTrail asserts:
//   - StartShift creates exactly one operator_sessions row.
//   - Each transition appends one operator_state_log row + one
//     event_outbox row.
//   - EndShift sets ended_at on the operator_sessions row.
//   - The first state_log row records `ready` with NULL reason; the
//     last records `offline`.
//   - The event_outbox rows match SubjectOpStateFor for every
//     transition.
func TestIntegration_PG_FullAuditTrail(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPGContainer(t)
	rdb := startRedisOnly(t)

	tenantID, userID, projectID := seedPrereqRows(t, pool)

	mach, err := fsm.New(fsm.Config{
		Redis:   rdb,
		PG:      pool,
		Outbox:  outbox.NewPostgresWriter(),
		Logger:  zaptest.NewLogger(t),
		Metrics: fsm.RegisterMetrics(prometheus.NewRegistry()),
	})
	require.NoError(t, err)

	ctx := context.Background()

	// 1. StartShift → ready
	_, err = mach.StartShift(ctx, api.StartShiftRequest{
		TenantID:   tenantID,
		OperatorID: userID,
		ProjectID:  projectID,
		ClientIP:   "127.0.0.1",
	})
	require.NoError(t, err)

	// 2. GoPause → pause
	_, err = mach.GoPause(ctx, api.GoPauseRequest{
		TenantID: tenantID, OperatorID: userID, Reason: "bio_break",
	})
	require.NoError(t, err)

	// 3. Resume → ready
	_, err = mach.Resume(ctx, tenantID, userID)
	require.NoError(t, err)

	// 4. EndShift → offline
	_, err = mach.EndShift(ctx, tenantID, userID)
	require.NoError(t, err)

	// Assertions:
	// (a) Exactly one operator_sessions row, with ended_at set. Read
	// via WithTenant — RLS-protected.
	var sessionCount int
	var endedAt *time.Time
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COUNT(*), MAX(ended_at)
			FROM operator_sessions
			WHERE tenant_id = $1 AND user_id = $2
		`, tenantID, userID).Scan(&sessionCount, &endedAt)
	}))
	require.Equal(t, 1, sessionCount, "exactly one operator_sessions row")
	require.NotNil(t, endedAt, "EndShift must set ended_at")

	// (b) operator_state_log rows: ready, pause, ready, offline (4 total).
	// operator_state_log policy cascades through operator_sessions, so
	// WithTenant resolves the visibility correctly.
	var stateLogStates []string
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT state FROM operator_state_log
			WHERE session_id = (
			  SELECT id FROM operator_sessions WHERE tenant_id = $1 AND user_id = $2
			)
			ORDER BY ts
		`, tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			stateLogStates = append(stateLogStates, s)
		}
		return rows.Err()
	}))
	require.Equal(t, []string{"ready", "pause", "ready", "offline"}, stateLogStates)

	// (c) event_outbox rows: 4 emissions, one per transition, all
	// targeting SubjectOpStateFor(tenant, operator). The outbox table
	// is owned by tenancy_admin and is not RLS-protected; read via
	// BypassRLS as the canonical platform-internal access.
	expectedSubject := api.SubjectOpStateFor(tenantID, userID)
	var subjects []string
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT subject FROM event_outbox
			WHERE tenant_id = $1 AND aggregate_id = $2
			ORDER BY id
		`, tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			subjects = append(subjects, s)
		}
		return rows.Err()
	}))
	require.Len(t, subjects, 4)
	for _, s := range subjects {
		require.Equal(t, expectedSubject, s)
	}
}

// TestIntegration_PG_ForceClosesSession verifies that Force(target=offline)
// closes the bound operator_sessions row and writes a `forced`-style
// state_log row with the supplied reason.
func TestIntegration_PG_ForceClosesSession(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	pool := startPGContainer(t)
	rdb := startRedisOnly(t)
	tenantID, userID, projectID := seedPrereqRows(t, pool)

	mach, err := fsm.New(fsm.Config{
		Redis:   rdb,
		PG:      pool,
		Outbox:  outbox.NewPostgresWriter(),
		Logger:  zaptest.NewLogger(t),
		Metrics: fsm.RegisterMetrics(prometheus.NewRegistry()),
	})
	require.NoError(t, err)

	ctx := context.Background()
	_, err = mach.StartShift(ctx, api.StartShiftRequest{
		TenantID: tenantID, OperatorID: userID, ProjectID: projectID,
	})
	require.NoError(t, err)

	// Force offline with a reason — heartbeat watchdog scenario.
	snap, err := mach.Force(ctx, tenantID, userID, api.StateOffline, api.ForceReasonHeartbeatLost)
	require.NoError(t, err)
	require.Equal(t, api.StateOffline, snap.State)

	// operator_sessions.ended_at must be set.
	var endedAt *time.Time
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT ended_at FROM operator_sessions
			WHERE tenant_id = $1 AND user_id = $2
		`, tenantID, userID).Scan(&endedAt)
	}))
	require.NotNil(t, endedAt)

	// state_log: ready, offline. The offline row carries the reason.
	type stateRow struct {
		state  string
		reason *string
	}
	var rowsOut []stateRow
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		rs, err := tx.Query(ctx, `
			SELECT state, reason FROM operator_state_log
			WHERE session_id = (
			  SELECT id FROM operator_sessions WHERE tenant_id = $1 AND user_id = $2
			)
			ORDER BY ts
		`, tenantID, userID)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var st string
			var rn *string
			if err := rs.Scan(&st, &rn); err != nil {
				return err
			}
			rowsOut = append(rowsOut, stateRow{state: st, reason: rn})
		}
		return rs.Err()
	}))
	require.Len(t, rowsOut, 2)
	require.Equal(t, "ready", rowsOut[0].state)
	require.Nil(t, rowsOut[0].reason)
	require.Equal(t, "offline", rowsOut[1].state)
	require.NotNil(t, rowsOut[1].reason)
	require.Equal(t, "heartbeat_lost", *rowsOut[1].reason)
}

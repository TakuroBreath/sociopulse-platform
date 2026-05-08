//go:build integration

// Integration tests for pkg/postgres against a real Postgres 16 instance
// booted via testcontainers-go. Run with:
//
//	go test -tags=integration -count=1 -timeout 5m ./pkg/postgres/...
//
// These tests verify the SET LOCAL app.tenant_id contract (Plan 03 Task 4):
//   - Open + Ping smoke test;
//   - WithTenant publishes app.tenant_id only inside its transaction;
//   - WithTenant rolls back on error;
//   - BypassRLS sets the tenancy_admin role.
//
// Unit-level coverage of behaviour that does not require a database (e.g.
// rejecting empty DSNs and zero UUIDs) lives in pool_unit_test.go.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sociopulse/platform/pkg/postgres"
)

// startPG boots Postgres 16 in a container and returns its DSN. The
// container is terminated automatically via t.Cleanup.
func startPG(t *testing.T) string {
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
	return dsn
}

func TestPool_OpenAndPing(t *testing.T) {
	dsn := startPG(t)
	ctx := context.Background()

	pool, err := postgres.Open(ctx, postgres.Config{
		DSN:            dsn,
		MaxConns:       4,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, pool.Ping(ctx))
}

func TestPool_WithTenant_SetsLocalSetting(t *testing.T) {
	dsn := startPG(t)
	ctx := context.Background()

	pool, err := postgres.Open(ctx, postgres.Config{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	var got string
	err = pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, "select current_setting('app.tenant_id', true)").Scan(&got)
	})
	require.NoError(t, err)
	require.Equal(t, tenantID.String(), got)
}

func TestPool_WithTenant_LocalScopedToTransaction(t *testing.T) {
	dsn := startPG(t)
	ctx := context.Background()
	pool, err := postgres.Open(ctx, postgres.Config{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	tenantID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return nil
	}))

	// Outside the WithTenant block, app.tenant_id must not be visible
	// (it was SET LOCAL only). Run a fresh query with no tenant context.
	var got string
	err = pool.RawQueryRow(ctx, "select coalesce(current_setting('app.tenant_id', true), '')").Scan(&got)
	require.NoError(t, err)
	require.Equal(t, "", got, "app.tenant_id leaked outside WithTenant transaction")
}

func TestPool_WithTenant_RollbackOnError(t *testing.T) {
	dsn := startPG(t)
	ctx := context.Background()
	pool, err := postgres.Open(ctx, postgres.Config{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Create a tiny test table that we control.
	_, err = pool.RawExec(ctx, "create table widgets (id int primary key)")
	require.NoError(t, err)

	tenantID := uuid.New()
	wantErr := "boom"
	err = pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, "insert into widgets (id) values (1)")
		require.NoError(t, err)
		return &fakeError{msg: wantErr}
	})
	require.Error(t, err)

	var count int
	require.NoError(t, pool.RawQueryRow(ctx, "select count(*) from widgets").Scan(&count))
	require.Zero(t, count, "WithTenant must rollback on error")
}

func TestPool_BypassRLS(t *testing.T) {
	dsn := startPG(t)
	ctx := context.Background()
	pool, err := postgres.Open(ctx, postgres.Config{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// tenancy_admin role created by 000001 init? not in this test container —
	// we open against an empty DB. So create the role + grant first.
	_, err = pool.RawExec(ctx, "create role tenancy_admin bypassrls")
	require.NoError(t, err)
	_, err = pool.RawExec(ctx, "grant tenancy_admin to test")
	require.NoError(t, err)

	var role string
	err = pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, "select current_user").Scan(&role)
	})
	require.NoError(t, err)
	// Postgres reports the SET LOCAL ROLE here; tenancy_admin acts as the role.
	require.Equal(t, "tenancy_admin", role)
}

// TestPool_LongLivedAcquire exercises the session-bound connection
// surface used by the dialer retry orchestrator's PgLeader. Tests:
//   - Acquire returns a usable conn that survives across multiple
//     statements on the same session.
//   - Release returns the conn to the pool; subsequent Exec on the
//     released *Conn errors (rather than silently mutating a different
//     pool member).
//   - The SAME session anchors a Postgres advisory lock — pg_try_advisory_lock
//     on the same conn is the production use case.
func TestPool_LongLivedAcquire(t *testing.T) {
	dsn := startPG(t)
	ctx := context.Background()
	pool, err := postgres.Open(ctx, postgres.Config{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Acquire a long-lived conn.
	conn, err := pool.LongLivedAcquire(ctx)
	require.NoError(t, err)
	require.NotNil(t, conn)

	// pg_backend_pid stays the same across calls on the same conn —
	// proves we have a STABLE underlying session.
	var pid1, pid2 int
	require.NoError(t, conn.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid1))
	require.NoError(t, conn.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid2))
	require.Equal(t, pid1, pid2, "two QueryRows on the same Conn must hit the same backend pid")

	// pg_try_advisory_lock on the conn — the production primitive the
	// dialer retry orchestrator uses for leader election.
	var got bool
	require.NoError(t, conn.QueryRow(ctx, "select pg_try_advisory_lock(42)").Scan(&got))
	require.True(t, got)

	// Release the conn (also releases the lock as a side effect of
	// session rebinding when the pool reaps idle conns; the explicit
	// pg_advisory_unlock would be cleaner — see retry/leader_election.go).
	conn.Release()

	// Subsequent calls on the released *Conn error rather than silently
	// hitting whatever conn pgxpool checked out next.
	_, err = conn.Exec(ctx, "select 1")
	require.Error(t, err)

	// Double-Release is a no-op.
	require.NotPanics(t, func() { conn.Release() })
}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

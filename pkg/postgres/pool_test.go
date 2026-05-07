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

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

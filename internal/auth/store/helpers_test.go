//go:build integration

// Test helpers shared between the auth store integration tests. The
// helpers boot Postgres 16 in a container, apply the project's full
// migration set (000001_init + 000002_outbox + 000003_users_auth_evolve),
// and yield a *postgres.Pool wired against it. The shape mirrors
// internal/tenancy/store/helpers_test.go so a developer flipping between
// the two test binaries finds the same affordances.

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // postgres driver for migrate
	_ "github.com/golang-migrate/migrate/v4/source/file"       // file:// source for migrate
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sociopulse/platform/pkg/postgres"
)

// newAuthTestPool boots Postgres 16 in a container, applies the project
// migrations through 000003, opens a *postgres.Pool against it, and
// registers t.Cleanup to tear everything down.
func newAuthTestPool(t *testing.T) *postgres.Pool {
	t.Helper()

	dsn := startPG(t)

	migrationsPath := repoMigrationsURL(t)
	m, err := migrate.New(migrationsPath, dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		srcErr, dbErr := m.Close()
		_ = srcErr
		_ = dbErr
	})
	require.NoError(t, m.Up())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

// startPG boots Postgres 16 in a container and returns its libpq DSN.
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

// repoMigrationsURL returns the file:// URL of the repo's migrations dir
// resolved relative to this test file.
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

// seedTenant inserts a tenants row using BypassRLS and returns its id.
// The auth/store integration tests need a real tenant FK target so
// users.tenant_id satisfies the existing reference constraint.
func seedTenant(t *testing.T, ctx context.Context, pool *postgres.Pool, orgCode string) uuid.UUID {
	t.Helper()
	const q = `
		INSERT INTO tenants (org_code, name, status, kms_kek_id, phone_hash_pepper)
		VALUES ($1, $2, 'active', 'yk-kek-test', $3)
		RETURNING id`

	var id uuid.UUID
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, q, orgCode, "Test "+orgCode, []byte("\x00\x01\x02\x03")).Scan(&id)
	}))
	return id
}

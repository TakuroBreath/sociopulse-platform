//go:build integration

// Test helpers shared between the store integration tests.
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
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sociopulse/platform/pkg/postgres"
)

// newTenancyTestPool boots Postgres 16 in a container, applies the project
// migrations, opens a *postgres.Pool against it, and registers t.Cleanup to
// tear everything down. Returns the pool ready for store-level integration
// tests.
func newTenancyTestPool(t *testing.T) *postgres.Pool {
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

// repoMigrationsURL returns the file:// URL of the repo's migrations dir.
// Resolved relative to this test file's location so tests don't depend on
// the test runner's CWD.
func repoMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	// here = .../internal/tenancy/store/helpers_test.go → repo = ../../../
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)

	_, err = os.Stat(abs)
	require.NoError(t, err)

	return "file://" + abs
}

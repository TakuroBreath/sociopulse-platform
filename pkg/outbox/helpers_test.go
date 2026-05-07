//go:build integration

// Test helpers shared between the integration tests in this package.
package outbox_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // "postgres" driver for migrate (matches cmd/migrator default)
	_ "github.com/golang-migrate/migrate/v4/source/file"       // file:// source for migrate
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sociopulse/platform/pkg/postgres"
)

// newOutboxTestPool boots Postgres 16 in a container, applies the project
// migrations (000001 + 000002), opens a *postgres.Pool against it, and
// registers t.Cleanup to tear everything down. It returns the pool ready
// for use by integration tests.
//
// The function is cheap to call from each test even though it spins up a
// container — testcontainers reuses the docker daemon and Postgres boots
// in well under a second on dev hardware.
func newOutboxTestPool(t *testing.T) *postgres.Pool {
	t.Helper()

	dsn := startPG(t)

	migrationsPath := repoMigrationsURL(t)
	m, err := migrate.New(migrationsPath, postgresMigrateDSN(dsn))
	require.NoError(t, err)
	t.Cleanup(func() {
		// Close releases migrate's own connection. Errors here only matter
		// for diagnostics — the container terminates immediately after.
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

// startPG boots Postgres 16 and returns a libpq DSN.
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
// Resolved relative to this test file's location so the test does not
// depend on the test runner's CWD.
func repoMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	// here = .../pkg/outbox/helpers_test.go → repo = ../../
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)

	// Verify the directory exists; otherwise migrate gives a confusing error.
	_, err = os.Stat(abs)
	require.NoError(t, err)

	return "file://" + abs
}

// postgresMigrateDSN translates a libpq DSN into the schema migrate's pgx/v5
// driver expects. testcontainers gives us "postgres://..." which migrate's
// pgx/v5 driver accepts directly, but other drivers want "pgx5://..." — keep
// this helper around so we can swap drivers later without touching tests.
func postgresMigrateDSN(dsn string) string { return dsn }

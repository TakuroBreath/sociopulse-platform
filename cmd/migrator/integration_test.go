//go:build integration

// Package main integration test: spins up Postgres 16 in a container and
// drives the migrator binary's run() end-to-end.
//
// Run: go test -tags=integration -count=1 -timeout 5m ./cmd/migrator/...
package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// Register the pgx/v5 stdlib driver under name "pgx" so database/sql can
	// dial Postgres for our verification queries (separate from the migrator
	// itself, which uses migrate's pgx/v5 driver).
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startPostgres boots Postgres 16 in a container and returns its DSN.
// Caller does not need to terminate; t.Cleanup handles it.
func startPostgres(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pg, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("sociopulse_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = pg.Terminate(context.Background())
	})

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	return dsn
}

// openDB wires database/sql to pgx/v5 (stdlib) for verification queries.
func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db
}

// writeStubMigration writes a minimal up/down pair into a temporary directory
// and returns its file:// URL. Used to exercise run() without depending on
// Plan 03 Task 3's migrations being committed yet.
func writeStubMigration(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "000001_init.up.sql"),
		[]byte("CREATE TABLE tenants (id BIGINT PRIMARY KEY);\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "000001_init.down.sql"),
		[]byte("DROP TABLE IF EXISTS tenants;\n"),
		0o644,
	))
	return "file://" + dir
}

func TestRun_UpAndStatus(t *testing.T) {
	t.Parallel()

	dsn := startPostgres(t)
	migrationsPath := writeStubMigration(t)

	require.NoError(t, run([]string{"up"}, dsn, migrationsPath, os.Stdout))

	db := openDB(t, dsn)

	var version int
	var dirty bool
	err := db.QueryRow("SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty)
	require.NoError(t, err)
	require.Equal(t, 1, version)
	require.False(t, dirty)

	// Confirm the stub migration's table actually exists.
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'tenants'
	`).Scan(&count))
	require.Equal(t, 1, count, "tenants table should be created by the up migration")

	// status sub-command should print the same version on stdout.
	out := &captureWriter{}
	require.NoError(t, run([]string{"status"}, dsn, migrationsPath, out))
	require.Contains(t, out.String(), "version=1")
	require.Contains(t, out.String(), "dirty=false")
}

func TestRun_Down(t *testing.T) {
	t.Parallel()

	dsn := startPostgres(t)
	migrationsPath := writeStubMigration(t)

	require.NoError(t, run([]string{"up"}, dsn, migrationsPath, os.Stdout))
	require.NoError(t, run([]string{"down"}, dsn, migrationsPath, os.Stdout))

	db := openDB(t, dsn)

	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'tenants'
	`).Scan(&count)
	require.NoError(t, err)
	require.Zero(t, count, "tenants table should be gone after down")
}

func TestRun_DownSteps(t *testing.T) {
	t.Parallel()

	dsn := startPostgres(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "000001_init.up.sql"),
		[]byte("CREATE TABLE a (id BIGINT PRIMARY KEY);\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "000001_init.down.sql"),
		[]byte("DROP TABLE a;\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "000002_b.up.sql"),
		[]byte("CREATE TABLE b (id BIGINT PRIMARY KEY);\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "000002_b.down.sql"),
		[]byte("DROP TABLE b;\n"),
		0o644,
	))
	migrationsPath := "file://" + dir

	require.NoError(t, run([]string{"up"}, dsn, migrationsPath, os.Stdout))

	db := openDB(t, dsn)
	var version int
	var dirty bool
	require.NoError(t, db.QueryRow("SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	require.Equal(t, 2, version)

	// Down by exactly one step should land at version 1.
	require.NoError(t, run([]string{"down", "--steps=1"}, dsn, migrationsPath, os.Stdout))

	require.NoError(t, db.QueryRow("SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	require.Equal(t, 1, version)
	require.False(t, dirty)
}

func TestRun_Force(t *testing.T) {
	t.Parallel()

	dsn := startPostgres(t)
	migrationsPath := writeStubMigration(t)

	// Simulate a dirty state: create the table golang-migrate uses and
	// stamp it with a bogus dirty version.
	db := openDB(t, dsn)
	_, err := db.Exec(`CREATE TABLE schema_migrations (version BIGINT PRIMARY KEY, dirty BOOLEAN NOT NULL)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (99, TRUE)`)
	require.NoError(t, err)

	require.NoError(t, run([]string{"force", "0"}, dsn, migrationsPath, os.Stdout))

	var version int
	var dirty bool
	err = db.QueryRow("SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty)
	require.NoError(t, err)
	require.Equal(t, 0, version)
	require.False(t, dirty)
}

func TestRun_StatusOnEmptyDB(t *testing.T) {
	t.Parallel()

	dsn := startPostgres(t)
	migrationsPath := writeStubMigration(t)

	out := &captureWriter{}
	require.NoError(t, run([]string{"status"}, dsn, migrationsPath, out))
	require.Contains(t, out.String(), "version=none")
	require.Contains(t, out.String(), "dirty=false")
}

func TestRun_ConnectionError_DistinctFromUsage(t *testing.T) {
	t.Parallel()

	// Connection errors are not usage errors: errors.As must yield false
	// so main() exits 2, not 1. Use a bogus DSN that's syntactically valid
	// but unreachable.
	dsn := "postgres://nope:nope@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1"
	migrationsPath := writeStubMigration(t)

	err := run([]string{"up"}, dsn, migrationsPath, os.Stdout)
	require.Error(t, err)

	var ue *usageError
	require.False(t, errors.As(err, &ue), "connection error must not be classified as usage")

	// Sanity: error chain mentions migrate-init, connect, or dial.
	msg := err.Error()
	require.True(t,
		strings.Contains(msg, "init migrate") ||
			strings.Contains(msg, "connect") ||
			strings.Contains(msg, "dial"),
		"expected a connection-flavoured error, got: %v", err,
	)
}

// captureWriter is a tiny io.Writer that buffers everything for assertions.
type captureWriter struct {
	buf []byte
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *captureWriter) String() string { return string(c.buf) }

//go:build integration

// Package main integration test for the ClickHouse target.
// Boots clickhouse-server in a container and drives run() against it.
//
// Run: go test -tags=integration -count=1 -timeout 5m ./cmd/migrator/...
package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	// Required for sql.Open("clickhouse", dsn) inside this test file
	// (the helper queries the schema_migrations table to verify state).
	// The migrator binary's CH driver registration in ch.go also brings
	// this in, but importing it here makes the test file's intent
	// self-contained and survives any future ch.go refactors.
	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
)

// chImage pins ClickHouse server to 24.8 — matches Yandex Managed CH
// supported version. Bumping this floats the test against whatever's
// :latest, breaking reproducibility.
const chImage = "clickhouse/clickhouse-server:24.8"

// chDSNs bundles the two DSNs a CH integration test needs:
//   - migrate: the DSN passed to run(); includes x-multi-statement=true,
//     which golang-migrate's clickhouse driver requires for multi-statement
//     migrations but which clickhouse-go itself rejects as an unknown setting.
//   - verify:  a plain DSN for sql.Open("clickhouse", …) verification queries.
type chDSNs struct {
	migrate string
	verify  string
}

// startClickHouse boots a ClickHouse container with predictable
// credentials and returns both DSNs (see chDSNs). t.Cleanup terminates
// the container; callers don't manage it.
func startClickHouse(t *testing.T) chDSNs {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	ch, err := tcclickhouse.Run(ctx, chImage,
		tcclickhouse.WithDatabase("sociopulse_test"),
		tcclickhouse.WithUsername("test"),
		tcclickhouse.WithPassword("test"),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = ch.Terminate(context.Background())
	})

	migrateDSN, err := ch.ConnectionString(ctx, "x-multi-statement=true")
	require.NoError(t, err)
	verifyDSN, err := ch.ConnectionString(ctx)
	require.NoError(t, err)
	return chDSNs{migrate: migrateDSN, verify: verifyDSN}
}

// openCHDB wires database/sql to the registered "clickhouse" driver for
// post-migration verification queries. Separate from the migrator's own
// connection (which is managed inside golang-migrate).
func openCHDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("clickhouse", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db
}

// writeStubCHMigration writes a minimal up/down pair into a temporary
// directory and returns its file:// URL. Used to exercise run() against
// CH without depending on Plan 13.1 Task 2's migrations being committed
// yet (this test landed FIRST as Task 1).
func writeStubCHMigration(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "000001_stub.up.sql"),
		[]byte("CREATE TABLE IF NOT EXISTS stub_table (id UInt64) ENGINE = MergeTree ORDER BY id;\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "000001_stub.down.sql"),
		[]byte("DROP TABLE IF EXISTS stub_table;\n"),
		0o644,
	))
	return "file://" + dir
}

// applyAllCHMigrations applies every migration in
// ../../migrations/clickhouse against the given DSN. Tests run from
// cmd/migrator/, so migrations/clickhouse is two directories up.
// The DSN passed in must be the migrate-flavoured one (with
// x-multi-statement=true) — see chDSNs.migrate.
func applyAllCHMigrations(t *testing.T, dsn string) {
	t.Helper()
	absMigrations, err := filepath.Abs(filepath.Join("..", "..", "migrations", "clickhouse"))
	require.NoError(t, err)
	require.NoError(t, run([]string{"up"}, dsn, "file://"+absMigrations, os.Stdout))
}

// TestRunCH_UpAndStatus_AppliesStubMigration drives a full up against a
// fresh CH container, then verifies (a) schema_migrations carries the
// applied version and is not dirty, (b) the stub table actually exists
// in the configured database. This is the headline integration test —
// failure here means the CH driver pipeline is broken end-to-end.
func TestRunCH_UpAndStatus_AppliesStubMigration(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	migrationsPath := writeStubCHMigration(t)

	require.NoError(t, run([]string{"up"}, dsns.migrate, migrationsPath, os.Stdout))

	db := openCHDB(t, dsns.verify)

	// schema_migrations on ClickHouse uses TinyLog (golang-migrate's
	// default), which is append-only — every state transition writes a
	// new row keyed by an auto-incrementing `sequence`. The current
	// state is the row with the highest sequence; that's how migrate's
	// own Version() reads it (database/clickhouse/clickhouse.go).
	var version uint64
	var dirty bool
	require.NoError(t, db.QueryRow(`
		SELECT version, dirty FROM schema_migrations
		ORDER BY sequence DESC LIMIT 1
	`).Scan(&version, &dirty))
	require.Equal(t, uint64(1), version)
	require.False(t, dirty)

	var count uint64
	require.NoError(t, db.QueryRow(`
		SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name = 'stub_table'
	`).Scan(&count))
	require.Equal(t, uint64(1), count)
}

// TestRunCH_Down_RemovesStubTable is the round-trip: apply, then revert.
// Verifies the down.sql is being driven, not just registered.
func TestRunCH_Down_RemovesStubTable(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	migrationsPath := writeStubCHMigration(t)

	require.NoError(t, run([]string{"up"}, dsns.migrate, migrationsPath, os.Stdout))
	require.NoError(t, run([]string{"down"}, dsns.migrate, migrationsPath, os.Stdout))

	db := openCHDB(t, dsns.verify)
	var count uint64
	require.NoError(t, db.QueryRow(`
		SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name = 'stub_table'
	`).Scan(&count))
	require.Zero(t, count, "stub_table should be gone after down")
}

// TestRunCH_ConnectionError_DistinctFromUsage guards the exit-code
// contract: a connection failure must yield a non-usage error so main()
// exits 2, not 1. Mirrors TestRun_ConnectionError_DistinctFromUsage on
// the Postgres side.
func TestRunCH_ConnectionError_DistinctFromUsage(t *testing.T) {
	t.Parallel()

	dsn := "clickhouse://nope:nope@127.0.0.1:1?database=nope&x-multi-statement=true"
	migrationsPath := writeStubCHMigration(t)

	err := run([]string{"up"}, dsn, migrationsPath, os.Stdout)
	require.Error(t, err)

	var ue *usageError
	require.False(t, errors.As(err, &ue), "connection error must not be classified as usage")
}

// TestSchema_EventsCalls_HasExpectedColumns asserts the engine,
// partition + sort keys, and full column shape of events_calls after
// applying the real migrations from migrations/clickhouse. Locked-in
// per master spec §6.4.
func TestSchema_EventsCalls_HasExpectedColumns(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	applyAllCHMigrations(t, dsns.migrate)
	db := openCHDB(t, dsns.verify)

	// Engine + partition + order key live on system.tables.
	var engine, partitionKey, sortingKey string
	require.NoError(t, db.QueryRow(`
		SELECT engine, partition_key, sorting_key
		FROM system.tables
		WHERE database = currentDatabase() AND name = 'events_calls'
	`).Scan(&engine, &partitionKey, &sortingKey))

	require.Equal(t, "MergeTree", engine)
	require.Equal(t, "toYYYYMM(date)", partitionKey)
	require.Equal(t, "tenant_id, project_id, ts", sortingKey)

	// Column types live on system.columns. Use a map for unordered
	// comparison; ORDER BY position is kept for readable failure output.
	rows, err := db.Query(`
		SELECT name, type FROM system.columns
		WHERE database = currentDatabase() AND table = 'events_calls'
		ORDER BY position
	`)
	require.NoError(t, err)
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var n, ty string
		require.NoError(t, rows.Scan(&n, &ty))
		got[n] = ty
	}
	require.NoError(t, rows.Err())

	want := map[string]string{
		"date":         "Date",
		"ts":           "DateTime64(3)",
		"tenant_id":    "UUID",
		"project_id":   "UUID",
		"operator_id":  "UUID",
		"call_id":      "UUID",
		"status":       "LowCardinality(String)",
		"duration_sec": "UInt32",
		"hangup_cause": "LowCardinality(String)",
		"region_code":  "LowCardinality(String)",
		"attempt_no":   "UInt8",
		"trunk_used":   "LowCardinality(String)",
		"event_id":     "UUID",
		"_inserted_at": "DateTime",
	}
	require.Equal(t, want, got)
}

// TestSchema_EventsOperatorState_HasExpectedColumns asserts the engine,
// partition + sort keys, and full column shape of events_operator_state.
// Locked-in per master spec §6.4.
func TestSchema_EventsOperatorState_HasExpectedColumns(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	applyAllCHMigrations(t, dsns.migrate)
	db := openCHDB(t, dsns.verify)

	var engine, partitionKey, sortingKey string
	require.NoError(t, db.QueryRow(`
		SELECT engine, partition_key, sorting_key
		FROM system.tables
		WHERE database = currentDatabase() AND name = 'events_operator_state'
	`).Scan(&engine, &partitionKey, &sortingKey))

	require.Equal(t, "MergeTree", engine)
	require.Equal(t, "toYYYYMM(date)", partitionKey)
	require.Equal(t, "tenant_id, user_id, ts", sortingKey)

	rows, err := db.Query(`
		SELECT name, type FROM system.columns
		WHERE database = currentDatabase() AND table = 'events_operator_state'
		ORDER BY position
	`)
	require.NoError(t, err)
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var n, ty string
		require.NoError(t, rows.Scan(&n, &ty))
		got[n] = ty
	}
	require.NoError(t, rows.Err())

	want := map[string]string{
		"date":                  "Date",
		"ts":                    "DateTime64(3)",
		"tenant_id":             "UUID",
		"user_id":               "UUID",
		"state":                 "LowCardinality(String)",
		"duration_in_state_sec": "UInt32",
		"project_id":            "Nullable(UUID)",
		"event_id":              "UUID",
		"_inserted_at":          "DateTime",
	}
	require.Equal(t, want, got)
}

// TestSchema_EventsRecordingUploaded_HasExpectedColumns asserts the
// engine, partition + sort keys, and full column shape of
// events_recording_uploaded. Out-of-spec relative to master §6.4 but
// required for the QC report use case in Plan 13.3 — see references Q4.
func TestSchema_EventsRecordingUploaded_HasExpectedColumns(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	applyAllCHMigrations(t, dsns.migrate)
	db := openCHDB(t, dsns.verify)

	var engine, partitionKey, sortingKey string
	require.NoError(t, db.QueryRow(`
		SELECT engine, partition_key, sorting_key
		FROM system.tables
		WHERE database = currentDatabase() AND name = 'events_recording_uploaded'
	`).Scan(&engine, &partitionKey, &sortingKey))

	require.Equal(t, "MergeTree", engine)
	require.Equal(t, "toYYYYMM(date)", partitionKey)
	require.Equal(t, "tenant_id, ts", sortingKey)

	rows, err := db.Query(`
		SELECT name, type FROM system.columns
		WHERE database = currentDatabase() AND table = 'events_recording_uploaded'
		ORDER BY position
	`)
	require.NoError(t, err)
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var n, ty string
		require.NoError(t, rows.Scan(&n, &ty))
		got[n] = ty
	}
	require.NoError(t, rows.Err())

	want := map[string]string{
		"date":                 "Date",
		"ts":                   "DateTime64(3)",
		"tenant_id":            "UUID",
		"project_id":           "UUID",
		"call_id":              "UUID",
		"fs_node":              "LowCardinality(String)",
		"s3_key":               "String",
		"size_bytes":           "UInt64",
		"duration_sec":         "UInt32",
		"encryption_key_alias": "LowCardinality(String)",
		"event_id":             "UUID",
		"_inserted_at":         "DateTime",
	}
	require.Equal(t, want, got)
}

// TestRunCH_UpIsIdempotent confirms that re-running `up` against an
// already-fully-migrated CH is a no-op (migrate.ErrNoChange swallowed
// by run()) and that the recorded version stays at 3 — which means the
// driver did not silently apply something extra on the second pass.
//
// schema_migrations on TinyLog is append-only; the latest applied
// version is the row with max sequence — same idiom as
// TestRunCH_UpAndStatus_AppliesStubMigration.
func TestRunCH_UpIsIdempotent(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	applyAllCHMigrations(t, dsns.migrate)
	applyAllCHMigrations(t, dsns.migrate) // re-apply: must be a no-op

	db := openCHDB(t, dsns.verify)

	var version uint64
	var dirty bool
	require.NoError(t, db.QueryRow(`
		SELECT version, dirty FROM schema_migrations
		ORDER BY sequence DESC LIMIT 1
	`).Scan(&version, &dirty))
	require.Equal(t, uint64(3), version, "expected version=3 after applying 000001..000003")
	require.False(t, dirty)
}

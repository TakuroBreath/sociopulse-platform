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
	"fmt"
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
// applying the real migrations from migrations/clickhouse.
//
// Schema baseline as of Plan 13.2.5 Task 4 (migration 000007): engine
// flipped from MergeTree to ReplacingMergeTree(_inserted_at) and
// ORDER BY changed to (tenant_id, event_id) so duplicate event_ids
// from cold-LRU restarts converge at merge time. The _inserted_at
// column type is also DateTime64(3) (was DateTime) so finer-grained
// "latest wins" works for sub-second redelivery storms.
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

	require.Equal(t, "ReplacingMergeTree", engine)
	require.Equal(t, "toYYYYMM(date)", partitionKey)
	require.Equal(t, "tenant_id, event_id", sortingKey)

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
		"_inserted_at": "DateTime64(3)",
	}
	require.Equal(t, want, got)
}

// TestSchema_EventsOperatorState_HasExpectedColumns asserts the engine,
// partition + sort keys, and full column shape of events_operator_state.
// Plan 13.2.5 Task 4 flipped this to ReplacingMergeTree(_inserted_at)
// ORDER BY (tenant_id, event_id) for the same reasons as events_calls.
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

	require.Equal(t, "ReplacingMergeTree", engine)
	require.Equal(t, "toYYYYMM(date)", partitionKey)
	require.Equal(t, "tenant_id, event_id", sortingKey)

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
		"_inserted_at":          "DateTime64(3)",
	}
	require.Equal(t, want, got)
}

// TestSchema_EventsRecordingUploaded_HasExpectedColumns asserts the
// engine, partition + sort keys, and full column shape of
// events_recording_uploaded. Plan 13.2.5 Task 4 flipped this to
// ReplacingMergeTree(_inserted_at) ORDER BY (tenant_id, event_id) for
// the same idempotency reasons as the other two source tables.
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

	require.Equal(t, "ReplacingMergeTree", engine)
	require.Equal(t, "toYYYYMM(date)", partitionKey)
	require.Equal(t, "tenant_id, event_id", sortingKey)

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
		"_inserted_at":         "DateTime64(3)",
	}
	require.Equal(t, want, got)
}

// TestRunCH_UpIsIdempotent confirms that re-running `up` against an
// already-fully-migrated CH is a no-op (migrate.ErrNoChange swallowed
// by run()) and that the recorded version stays at the latest — which
// means the driver did not silently apply something extra on the
// second pass.
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
	require.Equal(t, uint64(10), version, "expected version=10 after applying 000001..000010")
	require.False(t, dirty)
}

// TestMV_CallsHourly_RollupShape inserts six events_calls rows in a
// single hour-bucket (4 success, 2 fail), forces the MV-state merge
// via OPTIMIZE TABLE … FINAL, and reads the rollup back via sumMerge
// finals. AggregatingMergeTree state columns store *partial* state —
// reading them with sum() instead of sumMerge() returns garbage; the
// MV migration would silently look correct but every read would lie.
// This test pins the read pattern.
func TestMV_CallsHourly_RollupShape(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	applyAllCHMigrations(t, dsns.migrate)
	db := openCHDB(t, dsns.verify)

	const tenantStr = "11111111-1111-1111-1111-111111111111"
	const projectStr = "22222222-2222-2222-2222-222222222222"

	// 6 calls in the same hour-bucket: 4 success, 2 fail, region MSK.
	for i := range 6 {
		status := "success"
		if i >= 4 {
			status = "fail"
		}
		_, err := db.Exec(`
			INSERT INTO events_calls
			(date, ts, tenant_id, project_id, operator_id, call_id, status,
			 duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id)
			VALUES
			(toDate('2026-05-10'), toDateTime64('2026-05-10 12:30:00', 3),
			 toUUID(?), toUUID(?), generateUUIDv4(), generateUUIDv4(), ?,
			 60, 'NORMAL_CLEARING', 'MSK', 1, 'trunk-a', generateUUIDv4())
		`, tenantStr, projectStr, status)
		require.NoError(t, err)
	}

	_, err := db.Exec(`OPTIMIZE TABLE mv_calls_hourly_state FINAL`)
	require.NoError(t, err)

	var totalCalls, totalDur uint64
	require.NoError(t, db.QueryRow(`
		SELECT sumMerge(cnt), sumMerge(duration_sec)
		FROM mv_calls_hourly
		WHERE tenant_id = toUUID(?) AND project_id = toUUID(?)
		  AND bucket_hour >= toDateTime('2026-05-10 12:00:00')
		  AND bucket_hour <  toDateTime('2026-05-10 13:00:00')
	`, tenantStr, projectStr).Scan(&totalCalls, &totalDur))
	require.Equal(t, uint64(6), totalCalls, "6 calls inserted")
	require.Equal(t, uint64(360), totalDur, "6 calls × 60s each")
}

// TestMV_OperatorKpiDaily_AggregatesStatesAndCalls exercises the
// canonical CH two-feeder pattern: two MVs targeting the same
// AggregatingMergeTree state table, each contributing partial sumState
// columns. The events_calls feeder fills calls_total/success/refusal
// (with zero state for the seconds columns); the events_operator_state
// feeder fills talk/pause/ready/wrap_sec (with zero state for the
// calls columns). The same (tenant_id, user_id, project_id,
// bucket_date) tuple lets sumMerge collapse the two streams into one
// row.
func TestMV_OperatorKpiDaily_AggregatesStatesAndCalls(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	applyAllCHMigrations(t, dsns.migrate)
	db := openCHDB(t, dsns.verify)

	const tenantStr = "33333333-3333-3333-3333-333333333333"
	const operatorStr = "44444444-4444-4444-4444-444444444444"
	const projectStr = "55555555-5555-5555-5555-555555555555"

	// 3 calls (2 success, 1 refusal), each 60s.
	for _, status := range []string{"success", "success", "refusal"} {
		_, err := db.Exec(`
			INSERT INTO events_calls
			(date, ts, tenant_id, project_id, operator_id, call_id, status,
			 duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id)
			VALUES
			(toDate('2026-05-10'), toDateTime64('2026-05-10 10:00:00', 3),
			 toUUID(?), toUUID(?), toUUID(?), generateUUIDv4(), ?,
			 60, 'NORMAL_CLEARING', 'MSK', 1, 'trunk-a', generateUUIDv4())
		`, tenantStr, projectStr, operatorStr, status)
		require.NoError(t, err)
	}

	// Two operator_state rows: 600s in_call, 300s pause.
	_, err := db.Exec(`
		INSERT INTO events_operator_state
		(date, ts, tenant_id, user_id, state, duration_in_state_sec, project_id, event_id)
		VALUES
		(toDate('2026-05-10'), toDateTime64('2026-05-10 10:00:00', 3),
		 toUUID(?), toUUID(?), 'in_call', 600, toUUID(?), generateUUIDv4()),
		(toDate('2026-05-10'), toDateTime64('2026-05-10 10:30:00', 3),
		 toUUID(?), toUUID(?), 'pause',   300, toUUID(?), generateUUIDv4())
	`, tenantStr, operatorStr, projectStr, tenantStr, operatorStr, projectStr)
	require.NoError(t, err)

	_, err = db.Exec(`OPTIMIZE TABLE mv_operator_kpi_daily_state FINAL`)
	require.NoError(t, err)

	var calls, success, refusal, talk, pause uint64
	require.NoError(t, db.QueryRow(`
		SELECT sumMerge(calls_total),
		       sumMerge(calls_success),
		       sumMerge(calls_refusal),
		       sumMerge(talk_sec),
		       sumMerge(pause_sec)
		FROM mv_operator_kpi_daily
		WHERE tenant_id = toUUID(?) AND user_id = toUUID(?)
		  AND project_id = toUUID(?)
		  AND bucket_date = toDate('2026-05-10')
	`, tenantStr, operatorStr, projectStr).Scan(&calls, &success, &refusal, &talk, &pause))

	require.Equal(t, uint64(3), calls, "3 calls inserted")
	require.Equal(t, uint64(2), success, "2 successes")
	require.Equal(t, uint64(1), refusal, "1 refusal")
	require.Equal(t, uint64(600), talk, "600s in_call")
	require.Equal(t, uint64(300), pause, "300s pause")
}

// TestMV_QuotasProgress_RegionGroupedByDay inserts 80 calls across two
// regions (MSK and SPB) with distinct status mixes, then asserts the
// per-region/per-day rollup splits cleanly. Drives the §FR-I region
// progress dashboard.
func TestMV_QuotasProgress_RegionGroupedByDay(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	applyAllCHMigrations(t, dsns.migrate)
	db := openCHDB(t, dsns.verify)

	const tenantStr = "66666666-6666-6666-6666-666666666666"
	const projectStr = "77777777-7777-7777-7777-777777777777"

	fixtures := map[string]map[string]int{
		"MSK": {"success": 35, "fail": 10, "refusal": 5},
		"SPB": {"success": 20, "fail": 5, "refusal": 5},
	}
	for region, statusCounts := range fixtures {
		for status, count := range statusCounts {
			for range count {
				_, err := db.Exec(`
					INSERT INTO events_calls
					(date, ts, tenant_id, project_id, operator_id, call_id, status,
					 duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id)
					VALUES
					(toDate('2026-05-10'), toDateTime64('2026-05-10 14:00:00', 3),
					 toUUID(?), toUUID(?), generateUUIDv4(), generateUUIDv4(), ?,
					 60, 'NORMAL_CLEARING', ?, 1, 'trunk-a', generateUUIDv4())
				`, tenantStr, projectStr, status, region)
				require.NoError(t, err)
			}
		}
	}

	_, err := db.Exec(`OPTIMIZE TABLE mv_quotas_progress_state FINAL`)
	require.NoError(t, err)

	rows, err := db.Query(`
		SELECT region_code,
		       sumMerge(success_cnt),
		       sumMerge(fail_cnt),
		       sumMerge(refusal_cnt)
		FROM mv_quotas_progress
		WHERE tenant_id  = toUUID(?)
		  AND project_id = toUUID(?)
		  AND bucket_date = toDate('2026-05-10')
		GROUP BY region_code
		ORDER BY region_code
	`, tenantStr, projectStr)
	require.NoError(t, err)
	defer rows.Close()

	type row struct {
		region                 string
		success, fail, refusal uint64
	}
	var got []row
	for rows.Next() {
		var r row
		require.NoError(t, rows.Scan(&r.region, &r.success, &r.fail, &r.refusal))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())

	require.Equal(t, []row{
		{"MSK", 35, 10, 5},
		{"SPB", 20, 5, 5},
	}, got)
}

// TestMV_CallsHourly_RawVsMVParity is the canonical safeguard against
// MV-definition typos that would otherwise make the schema-shape tests
// look correct while every read returns lies. It inserts 100 calls
// across 4 hourly buckets and 3 statuses, then computes the same
// (count + duration sum) two ways: directly off events_calls, and
// through mv_calls_hourly via sumMerge. Both must agree exactly.
//
// Note on the timestamp parameter: CH's INSERT … VALUES parser only
// accepts truly-constant column expressions, so we format the per-row
// timestamp string in Go and pass it as a plain literal — using
// concat()/leftPad()/toString(?) inside toDateTime64() is rejected
// with "not a constant expression" even when the bound `?` is a plain
// integer.
func TestMV_CallsHourly_RawVsMVParity(t *testing.T) {
	t.Parallel()

	dsns := startClickHouse(t)
	applyAllCHMigrations(t, dsns.migrate)
	db := openCHDB(t, dsns.verify)

	const tenantStr = "88888888-8888-8888-8888-888888888888"
	const projectStr = "99999999-9999-9999-9999-999999999999"

	// 100 calls split across 4 hourly buckets and 3 statuses.
	for i := range 100 {
		ts := fmt.Sprintf("2026-05-10 %02d:00:00", i%4)
		status := []string{"success", "fail", "refusal"}[i%3]
		_, err := db.Exec(`
			INSERT INTO events_calls
			(date, ts, tenant_id, project_id, operator_id, call_id, status,
			 duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id)
			VALUES
			(toDate('2026-05-10'),
			 toDateTime64(?, 3),
			 toUUID(?), toUUID(?), generateUUIDv4(), generateUUIDv4(), ?,
			 ?, 'NORMAL_CLEARING', 'MSK', 1, 'trunk-a', generateUUIDv4())
		`, ts, tenantStr, projectStr, status, 30+i%10)
		require.NoError(t, err)
	}

	_, err := db.Exec(`OPTIMIZE TABLE mv_calls_hourly_state FINAL`)
	require.NoError(t, err)

	var rawCnt, rawDur uint64
	require.NoError(t, db.QueryRow(`
		SELECT count(), sum(toUInt64(duration_sec))
		FROM events_calls
		WHERE tenant_id = toUUID(?) AND project_id = toUUID(?)
		  AND date = toDate('2026-05-10')
	`, tenantStr, projectStr).Scan(&rawCnt, &rawDur))

	var mvCnt, mvDur uint64
	require.NoError(t, db.QueryRow(`
		SELECT sumMerge(cnt), sumMerge(duration_sec)
		FROM mv_calls_hourly
		WHERE tenant_id = toUUID(?) AND project_id = toUUID(?)
		  AND bucket_hour >= toDateTime('2026-05-10 00:00:00')
		  AND bucket_hour <  toDateTime('2026-05-11 00:00:00')
	`, tenantStr, projectStr).Scan(&mvCnt, &mvDur))

	require.Equal(t, rawCnt, mvCnt, "MV call count must match raw")
	require.Equal(t, rawDur, mvDur, "MV duration sum must match raw")
	require.Equal(t, uint64(100), mvCnt, "sanity: 100 calls inserted")
}

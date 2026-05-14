//go:build integration

package store_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/store"
	recordingapi "github.com/sociopulse/platform/internal/recording/api"
)

// openTestConn boots a fresh CH container, applies migrations, and
// returns a ready store.Conn. The container + Conn are reaped on
// t.Cleanup.
func openTestConn(t *testing.T) *store.Conn {
	t.Helper()
	dsns := startCH(t)
	migrateUp(t, dsns.migrate)

	conn, err := store.Open(t.Context(), store.Config{
		DSN:           dsns.verify,
		BatchSize:     100,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// buildFixtureCallEvents returns n call events for a single tenant /
// project, cycling through 5 success / 3 fail / 2 refusal when n==10.
// Per-row distinguishing fields (operator_id, call_id, event_id) are
// generated fresh so CH's primary key has no accidental collisions.
func buildFixtureCallEvents(tenantID, projectID uuid.UUID, n int) []apianalytics.AnalyticsCallEventPayload {
	statuses := []string{"success", "success", "success", "success", "success", "fail", "fail", "fail", "refusal", "refusal"}
	now := time.Now().UTC()
	rows := make([]apianalytics.AnalyticsCallEventPayload, 0, n)
	for i := range n {
		ts := now.Add(time.Duration(i) * time.Second)
		rows = append(rows, apianalytics.AnalyticsCallEventPayload{
			Date:        ts.Format("2006-01-02"),
			TS:          ts,
			TenantID:    tenantID,
			ProjectID:   projectID,
			OperatorID:  uuid.New(),
			CallID:      uuid.New(),
			Status:      statuses[i%len(statuses)],
			DurationSec: uint32(30 + i),
			HangupCause: "NORMAL_CLEARING",
			RegionCode:  "MSK",
			AttemptNo:   1,
			TrunkUsed:   "trunk-a",
			EventID:     uuid.New(),
		})
	}
	return rows
}

// TestInsertCalls_HappyPath inserts 10 fixture rows for a single
// tenant/project tuple and asserts CH reports the same count. The
// query filters by tenant_id so the test is isolated from any rows a
// parallel sibling may have inserted into a different tenant.
//
// This drives the canonical PrepareBatch → Append → Send pattern
// chosen in Plan 13.2 § Q5 over WithAsync. The fixture cycles through
// success/fail/refusal statuses to exercise LowCardinality binding.
func TestInsertCalls_HappyPath(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	rows := buildFixtureCallEvents(tenantID, projectID, 10)

	require.NoError(t, store.InsertCalls(t.Context(), conn, rows))

	var got uint64
	require.NoError(t, conn.Driver().QueryRow(t.Context(),
		"SELECT count() FROM events_calls WHERE tenant_id = ?", tenantID).Scan(&got))
	require.Equal(t, uint64(10), got)
}

// TestInsertOperatorStates_HappyPath inserts 5 state transitions, one
// of which carries a nil ProjectID. The nil-ProjectID row exercises
// the CH Nullable(UUID) binding and the assertion below confirms it
// appears as a SQL NULL (CH returns 1 for isNull).
func TestInsertOperatorStates_HappyPath(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantID := uuid.New()
	userID := uuid.New()
	projectID := uuid.New()
	now := time.Now().UTC()

	rows := []apianalytics.AnalyticsOperatorStateEventPayload{
		{Date: now.Format("2006-01-02"), TS: now, TenantID: tenantID, UserID: userID, State: "ready", DurationInStateSec: 0, ProjectID: &projectID, EventID: uuid.New()},
		{Date: now.Format("2006-01-02"), TS: now.Add(1 * time.Second), TenantID: tenantID, UserID: userID, State: "dialing", DurationInStateSec: 5, ProjectID: &projectID, EventID: uuid.New()},
		{Date: now.Format("2006-01-02"), TS: now.Add(2 * time.Second), TenantID: tenantID, UserID: userID, State: "in_call", DurationInStateSec: 60, ProjectID: &projectID, EventID: uuid.New()},
		{Date: now.Format("2006-01-02"), TS: now.Add(3 * time.Second), TenantID: tenantID, UserID: userID, State: "wrap_up", DurationInStateSec: 20, ProjectID: &projectID, EventID: uuid.New()},
		// NULL project — operator went offline; no project context.
		{Date: now.Format("2006-01-02"), TS: now.Add(4 * time.Second), TenantID: tenantID, UserID: userID, State: "offline", DurationInStateSec: 0, ProjectID: nil, EventID: uuid.New()},
	}

	require.NoError(t, store.InsertOperatorStates(t.Context(), conn, rows))

	var total uint64
	require.NoError(t, conn.Driver().QueryRow(t.Context(),
		"SELECT count() FROM events_operator_state WHERE tenant_id = ?", tenantID).Scan(&total))
	require.Equal(t, uint64(5), total)

	var nullCount uint64
	require.NoError(t, conn.Driver().QueryRow(t.Context(),
		"SELECT count() FROM events_operator_state WHERE tenant_id = ? AND isNull(project_id)", tenantID).Scan(&nullCount))
	require.Equal(t, uint64(1), nullCount, "exactly one NULL project_id row")
}

// TestInsertRecordingsUploaded_HappyPath inserts 3 fixture recording
// events. The CH date+ts columns are derived inside the batch helper
// from RecordingUploadedEvent.CommittedAt (unix seconds); the helper
// also clamps duration_sec via the producer-side invariant established
// in Plan 13.2 Task 1 (DurationSec ≥ 0).
func TestInsertRecordingsUploaded_HappyPath(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	now := time.Now().UTC()

	rows := []recordingapi.RecordingUploadedEvent{
		{
			RecordingID: uuid.New(), CallID: uuid.New(), TenantID: tenantID, ProjectID: projectID,
			FSNode: "fs-01", S3Key: "tenant/" + tenantID.String() + "/call/x.opus.enc",
			EncryptionKeyAlias: "kms-alias-tenant", EventID: uuid.New(),
			BytesSize: 12345, DurationMS: 60_000, DurationSec: 60,
			SHA256Hex: "abc123", Status: "stored", CommittedAt: now.Unix(),
		},
		{
			RecordingID: uuid.New(), CallID: uuid.New(), TenantID: tenantID, ProjectID: projectID,
			FSNode: "fs-02", S3Key: "tenant/" + tenantID.String() + "/call/y.opus.enc",
			EncryptionKeyAlias: "kms-alias-tenant", EventID: uuid.New(),
			BytesSize: 23456, DurationMS: 90_000, DurationSec: 90,
			SHA256Hex: "def456", Status: "stored", CommittedAt: now.Unix() + 1,
		},
		{
			RecordingID: uuid.New(), CallID: uuid.New(), TenantID: tenantID, ProjectID: projectID,
			FSNode: "fs-01", S3Key: "tenant/" + tenantID.String() + "/call/z.opus.enc",
			EncryptionKeyAlias: "kms-alias-tenant", EventID: uuid.New(),
			BytesSize: 34567, DurationMS: 120_000, DurationSec: 120,
			SHA256Hex: "789abc", Status: "stored", CommittedAt: now.Unix() + 2,
		},
	}

	require.NoError(t, store.InsertRecordingsUploaded(t.Context(), conn, rows))

	var got uint64
	require.NoError(t, conn.Driver().QueryRow(t.Context(),
		"SELECT count() FROM events_recording_uploaded WHERE tenant_id = ?", tenantID).Scan(&got))
	require.Equal(t, uint64(3), got)

	// Sanity: duration_sec sum 60+90+120 == 270.
	var totalDuration uint64
	require.NoError(t, conn.Driver().QueryRow(t.Context(),
		"SELECT sum(duration_sec) FROM events_recording_uploaded WHERE tenant_id = ?", tenantID).Scan(&totalDuration))
	require.Equal(t, uint64(270), totalDuration)
}

// TestSchemaShape_PayloadFieldsMatchCHColumns is the drift-guard
// between the Task 1 payload types and the CH schema. For each of the
// three (payload-type, ch-table) pairs we collect the JSON tag set on
// the Go side and the column-name set on the CH side, then assert set
// equality. Adding a CH column without extending the payload — or
// vice versa — fails this test loudly.
//
// The system.columns filter drops auto-default columns (_inserted_at
// is DEFAULT now()) so the payload tag set, which omits it, matches.
//
// The recording row uses an explicit tag list (recordingPayloadCHTags)
// because the full RecordingUploadedEvent payload carries extra
// fields (recording_id / duration_ms / sha256 / status) that exist on
// the bus but NOT in CH — the table records the analytic projection
// only. The explicit list pins exactly which JSON tags should map.
func TestSchemaShape_PayloadFieldsMatchCHColumns(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	cases := []struct {
		name     string
		table    string
		wantTags []string
	}{
		{
			name:     "events_calls",
			table:    "events_calls",
			wantTags: jsonTagsOf(apianalytics.AnalyticsCallEventPayload{}),
		},
		{
			name:     "events_operator_state",
			table:    "events_operator_state",
			wantTags: jsonTagsOf(apianalytics.AnalyticsOperatorStateEventPayload{}),
		},
		{
			name:     "events_recording_uploaded",
			table:    "events_recording_uploaded",
			wantTags: recordingPayloadCHTags(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := chColumns(t, conn, tc.table)
			require.ElementsMatch(t, tc.wantTags, got,
				"payload JSON tags MUST match CH columns (table=%s)", tc.table)
		})
	}
}

// chColumns returns the non-default column names of the given table
// from system.columns. The default_kind != 'DEFAULT' filter excludes
// _inserted_at (declared as `DateTime DEFAULT now()` in the
// migrations) so the comparison aligns with the Go payload structs,
// which do not carry that column.
func chColumns(t *testing.T, conn *store.Conn, table string) []string {
	t.Helper()
	rows, err := conn.Driver().Query(t.Context(),
		`SELECT name FROM system.columns
		 WHERE database = currentDatabase()
		   AND table = ?
		   AND default_kind != 'DEFAULT'`,
		table)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		got = append(got, n)
	}
	require.NoError(t, rows.Err())
	return got
}

// jsonTagsOf walks the exported fields of v via reflection and
// returns each field's `json:"<tag>"` value. Comma-delimited options
// (`,omitempty`) are stripped. Fields without a json tag are skipped
// — un-tagged exported fields are not part of the wire contract.
func jsonTagsOf(v any) []string {
	rt := reflect.TypeOf(v)
	out := make([]string, 0, rt.NumField())
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag, ok := f.Tag.Lookup("json")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// recordingPayloadCHTags is the explicit list of CH-relevant JSON
// tags on RecordingUploadedEvent. Extending the CH schema requires
// extending this list AND the InsertRecordingsUploaded helper.
func recordingPayloadCHTags() []string {
	return []string{
		"date", "ts", "tenant_id", "project_id", "call_id",
		"fs_node", "s3_key", "size_bytes", "duration_sec",
		"encryption_key_alias", "event_id",
	}
}

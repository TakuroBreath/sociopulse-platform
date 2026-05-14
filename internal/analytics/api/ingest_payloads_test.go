package api_test

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/analytics/api"
)

// TestAnalyticsCallEventPayload_JSONKeySet locks the column-exact JSON
// shape for the analytics.event.calls subject. The ingest pipeline maps
// each key to the matching column in migrations/clickhouse/000001_events_calls.up.sql.
// A drift in either direction (Go struct or CH column) is a silent data
// loss bug — keep this test loud.
func TestAnalyticsCallEventPayload_JSONKeySet(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	projectID := uuid.New()
	operatorID := uuid.New()
	callID := uuid.New()
	eventID := uuid.New()
	ts := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	p := api.AnalyticsCallEventPayload{
		Date:        "2026-05-14",
		TS:          ts,
		TenantID:    tenantID,
		ProjectID:   projectID,
		OperatorID:  operatorID,
		CallID:      callID,
		Status:      "success",
		DurationSec: 42,
		HangupCause: "NORMAL_CLEARING",
		RegionCode:  "RU-MOW",
		AttemptNo:   2,
		TrunkUsed:   "trunk-a",
		EventID:     eventID,
	}

	raw, err := json.Marshal(p)
	require.NoError(t, err)

	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))

	gotKeys := make([]string, 0, len(m))
	for k := range m {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)

	wantKeys := []string{
		"attempt_no",
		"call_id",
		"date",
		"duration_sec",
		"event_id",
		"hangup_cause",
		"operator_id",
		"project_id",
		"region_code",
		"status",
		"tenant_id",
		"trunk_used",
		"ts",
	}
	require.Equal(t, wantKeys, gotKeys,
		"AnalyticsCallEventPayload JSON keys must match events_calls CH columns exactly")
}

// TestAnalyticsOperatorStateEventPayload_JSONKeySet locks the column-exact
// JSON shape for the analytics.event.operator_state subject. Keys mirror
// migrations/clickhouse/000002_events_operator_state.up.sql.
func TestAnalyticsOperatorStateEventPayload_JSONKeySet(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userID := uuid.New()
	projectID := uuid.New()
	eventID := uuid.New()
	ts := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	p := api.AnalyticsOperatorStateEventPayload{
		Date:               "2026-05-14",
		TS:                 ts,
		TenantID:           tenantID,
		UserID:             userID,
		State:              "ready",
		DurationInStateSec: 30,
		ProjectID:          &projectID,
		EventID:            eventID,
	}

	raw, err := json.Marshal(p)
	require.NoError(t, err)

	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))

	gotKeys := make([]string, 0, len(m))
	for k := range m {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)

	wantKeys := []string{
		"date",
		"duration_in_state_sec",
		"event_id",
		"project_id",
		"state",
		"tenant_id",
		"ts",
		"user_id",
	}
	require.Equal(t, wantKeys, gotKeys,
		"AnalyticsOperatorStateEventPayload JSON keys must match events_operator_state CH columns exactly")
}

// TestAnalyticsOperatorStateEventPayload_NilProjectMarshalsNull documents
// the Nullable(UUID) round-trip: a nil ProjectID must marshal to JSON null
// so the CH ingester binds NULL rather than a sentinel uuid.
func TestAnalyticsOperatorStateEventPayload_NilProjectMarshalsNull(t *testing.T) {
	t.Parallel()

	p := api.AnalyticsOperatorStateEventPayload{
		Date:               "2026-05-14",
		TS:                 time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		TenantID:           uuid.New(),
		UserID:             uuid.New(),
		State:              "offline",
		DurationInStateSec: 0,
		ProjectID:          nil,
		EventID:            uuid.New(),
	}

	raw, err := json.Marshal(p)
	require.NoError(t, err)

	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Contains(t, m, "project_id")
	require.JSONEq(t, "null", string(m["project_id"]))
}

package api

import (
	"time"

	"github.com/google/uuid"
)

// AnalyticsCallEventPayload is the cross-tenant payload published on
// the analytics.event.calls subject. Field order, JSON tags, and Go types
// MUST mirror the column tuple of migrations/clickhouse/000001_events_calls.up.sql.
//
// The analytics ingester (Plan 13.2 Task 2) decodes this payload and
// binds positional values into a PrepareBatch INSERT against events_calls.
// Drift between this struct and the CH schema is a silent data-loss bug —
// the JSON-shape test in ingest_payloads_test.go guards the key set; the
// schema-shape tests in cmd/migrator guard the CH side.
//
// Field semantics (mirroring Plan 13.2 § Q4–Q9 decisions):
//   - Status: dialer-side wrap-up disposition (success / refused / ...).
//   - HangupCause: SIP cause string; v1 ingester accepts "" sentinel
//     because FSM commit lacks FreeSWITCH-side cause data (Q8).
//   - RegionCode: respondent's region; v1 ingester accepts "" sentinel
//     because the FSM Machine does not currently carry respondent data (Q9).
//   - TrunkUsed: trunk identifier set by the telephony router; "" when
//     unknown.
//   - AttemptNo: 1-based attempt counter; 1 when retry orchestration has
//     not surfaced a higher value on the Machine.
type AnalyticsCallEventPayload struct {
	Date        string    `json:"date"`         // YYYY-MM-DD → CH Date
	TS          time.Time `json:"ts"`           // CH DateTime64(3)
	TenantID    uuid.UUID `json:"tenant_id"`    // CH UUID
	ProjectID   uuid.UUID `json:"project_id"`   // CH UUID
	OperatorID  uuid.UUID `json:"operator_id"`  // CH UUID
	CallID      uuid.UUID `json:"call_id"`      // CH UUID
	Status      string    `json:"status"`       // CH LowCardinality(String)
	DurationSec uint32    `json:"duration_sec"` // CH UInt32
	HangupCause string    `json:"hangup_cause"` // CH LowCardinality(String)
	RegionCode  string    `json:"region_code"`  // CH LowCardinality(String)
	AttemptNo   uint8     `json:"attempt_no"`   // CH UInt8
	TrunkUsed   string    `json:"trunk_used"`   // CH LowCardinality(String)
	EventID     uuid.UUID `json:"event_id"`     // CH UUID (dedup key)
}

// AnalyticsOperatorStateEventPayload is the cross-tenant payload published
// on the analytics.event.operator_state subject. Field order, JSON tags,
// and Go types MUST mirror the column tuple of
// migrations/clickhouse/000002_events_operator_state.up.sql.
//
// Notable shape difference vs AnalyticsCallEventPayload:
//   - ProjectID is *uuid.UUID — the CH column is Nullable(UUID) because
//     transitions to / from `offline` may carry no project context.
//     A nil pointer marshals to JSON null.
//   - DurationInStateSec is the time spent in the PREVIOUS state
//     (delta from previous operator_state_log row's ts to this row's ts).
//     0 when there is no prior log row (start of session).
type AnalyticsOperatorStateEventPayload struct {
	Date               string     `json:"date"`                  // YYYY-MM-DD → CH Date
	TS                 time.Time  `json:"ts"`                    // CH DateTime64(3)
	TenantID           uuid.UUID  `json:"tenant_id"`             // CH UUID
	UserID             uuid.UUID  `json:"user_id"`               // CH UUID
	State              string     `json:"state"`                 // CH LowCardinality(String)
	DurationInStateSec uint32     `json:"duration_in_state_sec"` // CH UInt32
	ProjectID          *uuid.UUID `json:"project_id"`            // CH Nullable(UUID)
	EventID            uuid.UUID  `json:"event_id"`              // CH UUID (dedup key)
}

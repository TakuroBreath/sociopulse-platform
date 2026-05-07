package api

import (
	"fmt"

	"github.com/google/uuid"
)

// NATS subject placeholders for the durable JetStream stream DIALER
// (24 h retention, via outbox).
//
// The subject constants below contain literal "<t>", "<op_id>", and
// "<call_id>" placeholders; the runtime materialises concrete subjects
// via the Subject<X>For helpers.
const (
	// SubjectOpState is published on every FSM transition (operator state log).
	SubjectOpState = "tenant.<t>.dialer.op.<op_id>.state"
	// SubjectCallLifecycle is published on dialer-level call lifecycle changes.
	SubjectCallLifecycle = "tenant.<t>.dialer.call.<call_id>.lifecycle"
	// SubjectCallFinalized is published when a call's terminal state is set.
	// Includes cost-bearing fields. Consumed by analytics + billing.
	SubjectCallFinalized = "tenant.<t>.dialer.call.finalized"

	// SubjectAnalyticsCalls is the denormalised call event for ClickHouse.
	SubjectAnalyticsCalls = "analytics.event.calls"
	// SubjectAnalyticsOperatorState is the denormalised operator state row for ClickHouse.
	SubjectAnalyticsOperatorState = "analytics.event.operator_state"
)

// SubjectOpStateFor returns the concrete subject for an operator FSM
// transition for the given tenant/operator.
func SubjectOpStateFor(tenantID, operatorID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.dialer.op.%s.state", tenantID, operatorID)
}

// SubjectCallLifecycleFor returns the concrete subject for a per-call dialer
// lifecycle event.
func SubjectCallLifecycleFor(tenantID, callID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.dialer.call.%s.lifecycle", tenantID, callID)
}

// SubjectCallFinalizedFor returns the concrete subject for the dialer.call.finalized
// event for the given tenant.
func SubjectCallFinalizedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.dialer.call.finalized", tenantID)
}

// asynq task type constants.
const (
	// TaskRetryDue is the every-30-second task that re-enqueues mature retries.
	TaskRetryDue = "dialer.retry_due"
)

// OperatorStateChangedEvent is the payload for SubjectOpState.
// Mirrors a Snapshot but keeps the field set tight — analytics also
// consumes this directly via the operator.state.changed kind.
type OperatorStateChangedEvent struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	OperatorID  uuid.UUID `json:"operator_id"`
	State       State     `json:"state"`
	ChangedAt   int64     `json:"changed_at"` // unix millis
	ProjectID   string    `json:"project_id,omitempty"`
	CallID      string    `json:"call_id,omitempty"`
	PauseReason string    `json:"pause_reason,omitempty"`
}

// CallLifecycleEvent is the payload for SubjectCallLifecycle.
type CallLifecycleEvent struct {
	CallID     uuid.UUID `json:"call_id"`
	OperatorID uuid.UUID `json:"operator_id"`
	Stage      string    `json:"stage"` // start | answer | hangup
	OccurredAt int64     `json:"occurred_at"`
}

// CallFinalizedEvent is the payload for SubjectCallFinalized.
// Consumed by analytics + billing. Cost-bearing fields are included.
type CallFinalizedEvent struct {
	CallID       uuid.UUID `json:"call_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	OperatorID   uuid.UUID `json:"operator_id"`
	ProjectID    uuid.UUID `json:"project_id"`
	RespondentID uuid.UUID `json:"respondent_id"`
	TrunkUsed    string    `json:"trunk_used"`
	DurationSec  int32     `json:"duration_sec"`
	Status       string    `json:"status"`
	StorageBytes int64     `json:"storage_bytes"`
	FinalizedAt  int64     `json:"finalized_at"` // unix seconds
}

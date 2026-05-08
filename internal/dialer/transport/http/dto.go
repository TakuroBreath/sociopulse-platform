package http

import (
	"time"

	"github.com/google/uuid"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
)

// StartShiftDTO is the body of POST /api/sessions/start. The
// operator's TenantID and OperatorID are sourced from the JWT claims —
// only the project the operator wishes to bind to is on the wire.
type StartShiftDTO struct {
	ProjectID uuid.UUID `json:"project_id" binding:"required"`
}

// GoPauseDTO is the body of POST /api/sessions/pause.
type GoPauseDTO struct {
	Reason string `json:"reason" binding:"required,min=1,max=64"`
}

// SubmitStatusDTO is the body of POST /api/calls/:id/status.
//
// Status is constrained to the operator-visible disposition vocabulary
// — server-side rules (status_rules table) authoritatively decide
// retry / DNC bucketing; the wire enum is just the syntactic gate.
type SubmitStatusDTO struct {
	CallID       uuid.UUID `json:"call_id" binding:"required"`
	RespondentID uuid.UUID `json:"respondent_id" binding:"required"`
	Status       string    `json:"status" binding:"required,oneof=success refused wrong_person dropped no_answer busy callback tech_failure"`
	Comment      string    `json:"comment" binding:"max=2000"`
}

// HangupDTO is the body of POST /api/calls/:id/hangup. Reason is
// optional — empty string maps to "operator_hangup" downstream.
type HangupDTO struct {
	Reason string `json:"reason" binding:"max=64"`
}

// ForceDTO is the body of POST /api/operator/:id/force. Both fields
// are validated via api.State.Valid / api.ForceReason.Valid before we
// dispatch to the FSM so a bogus enum is rejected with 400 rather than
// surfacing as ErrInvalidTransition.
type ForceDTO struct {
	Target dialerapi.State       `json:"target" binding:"required"`
	Reason dialerapi.ForceReason `json:"reason" binding:"required"`
}

// SnapshotDTO is the wire shape for an operator FSM Snapshot. We do
// not marshal api.Snapshot directly so future additions to the api
// type don't silently change the public response.
type SnapshotDTO struct {
	State          string     `json:"state"`
	StateEnteredAt time.Time  `json:"state_entered_at"`
	ProjectID      *uuid.UUID `json:"project_id,omitempty"`
	CurrentCallID  *uuid.UUID `json:"current_call_id,omitempty"`
	RespondentID   *uuid.UUID `json:"respondent_id,omitempty"`
	PauseReason    *string    `json:"pause_reason,omitempty"`
	HeartbeatAt    time.Time  `json:"heartbeat_at"`
}

// StartShiftResponse is the body of POST /api/sessions/start. We
// supplement the bare Snapshot with a "next allowed at" hint sourced
// from WorkingHoursChecker so the UI can show the operator a
// short-circuit message rather than firing the dispatch loop blindly.
type StartShiftResponse struct {
	Snapshot       SnapshotDTO `json:"snapshot"`
	NextAllowedAt  *time.Time  `json:"next_allowed_at,omitempty"`
	OutsideAllowed bool        `json:"outside_allowed,omitempty"`
}

// ErrorEnvelope is the JSON shape every 4xx/5xx response uses. Mirrors
// the auth + crm packages' envelope so the wire format stays uniform
// across modules. `code` is the dotted, low-cardinality sentinel
// identifier; `message` is the user-facing detail (scrubbed for 5xx).
type ErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// snapshotToDTO converts an api.Snapshot to its wire representation.
// Caller-supplied input — no validation here, the DTO simply mirrors
// the stored fields. Pointer fields stay nil when absent so the JSON
// stays sparse.
func snapshotToDTO(s dialerapi.Snapshot) SnapshotDTO {
	return SnapshotDTO{
		State:          string(s.State),
		StateEnteredAt: s.StateEnteredAt,
		ProjectID:      s.ProjectID,
		CurrentCallID:  s.CurrentCallID,
		RespondentID:   s.RespondentID,
		PauseReason:    s.PauseReason,
		HeartbeatAt:    s.HeartbeatAt,
	}
}

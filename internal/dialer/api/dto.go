// dto.go declares the data-transfer types shared between the dialer module
// and its consumers. The package-level documentation lives in doc.go.
package api

import (
	"time"

	"github.com/google/uuid"
)

// State is one operator FSM state.
type State string

const (
	StateOffline State = "offline"
	StateReady   State = "ready"
	StateDialing State = "dialing"
	StateCall    State = "call"
	StateStatus  State = "status"
	StateVerify  State = "verify"
	StatePause   State = "pause"
)

// Valid reports whether s is one of the recognized State enum values.
//
// Used by:
//   - the FSM's Force() escape hatch to reject garbage target states from
//     supervisor / watchdog inputs before touching Redis;
//   - the store's hash-deserialiser to detect a corrupt row.
//
// Adding a new State constant requires extending this switch — that
// requirement is enforced by the exhaustive test in state_test.go.
func (s State) Valid() bool {
	switch s {
	case StateOffline, StateReady, StateDialing, StateCall,
		StateStatus, StateVerify, StatePause:
		return true
	}
	return false
}

// Event is one operator FSM transition trigger.
type Event string

const (
	EventStartShift      Event = "start_shift"
	EventEndShift        Event = "end_shift"
	EventGoReady         Event = "go_ready"
	EventGoPause         Event = "go_pause"
	EventResume          Event = "resume"
	EventCallStarted     Event = "call_started"
	EventCallEnded       Event = "call_ended"
	EventCallFailed      Event = "call_failed"
	EventStatusSubmitted Event = "status_submitted"
	EventGoVerify        Event = "go_verify"
	EventVerifyDone      Event = "verify_done"
	EventForceOffline    Event = "force_offline"
)

// Valid reports whether e is one of the recognized Event enum values.
//
// Used at the HTTP boundary so an unknown JSON Event string is rejected
// with a 400 before the request reaches the FSM. Adding a new Event
// constant requires extending this switch — exhaustively tested in
// state_test.go.
func (e Event) Valid() bool {
	switch e {
	case EventStartShift, EventEndShift, EventGoReady, EventGoPause,
		EventResume, EventCallStarted, EventCallEnded, EventCallFailed,
		EventStatusSubmitted, EventGoVerify, EventVerifyDone,
		EventForceOffline:
		return true
	}
	return false
}

// ForceReason enumerates the supervisor- / watchdog-driven reasons for
// Machine.Force. The label is fed into Prometheus
// dialer_fsm_force_total{reason} and MUST stay low-cardinality.
//
// Adding a new constant requires extending Valid() and is enforced by the
// exhaustive test in state_test.go.
type ForceReason string

const (
	ForceReasonHeartbeatLost  ForceReason = "heartbeat_lost"  // watchdog: presence TTL expired
	ForceReasonSupervisorKick ForceReason = "supervisor_kick" // operator kicked by supervisor UI
	ForceReasonShutdown       ForceReason = "shutdown"        // graceful drain
	ForceReasonAdminOverride  ForceReason = "admin_override"  // catch-all for admin tooling
	ForceReasonOther          ForceReason = "other"           // unrecognised free-form input bucket
)

// Valid reports whether r is a recognized ForceReason. Used by Force to
// gate Prometheus labels — unknown reasons are bucketed under "other"
// rather than blown up into per-instance label values.
func (r ForceReason) Valid() bool {
	switch r {
	case ForceReasonHeartbeatLost, ForceReasonSupervisorKick,
		ForceReasonShutdown, ForceReasonAdminOverride, ForceReasonOther:
		return true
	}
	return false
}

// StatusOutcome classifies the result of a completed call attempt. It
// flows from RecordCallEnded into the operator's Snapshot for the
// `status` state, where it is consulted by the FSM transition
// (status, go_verify) — per CONTEXT.md, `verify` is reachable only from
// success-class outcomes. The label is low-cardinality and safe to ship
// into Prometheus metrics.
//
// Adding a new constant requires extending Valid() and IsSuccessClass()
// (the latter governs the (status, go_verify) gate).
type StatusOutcome string

const (
	// OutcomeSuccess marks an answered, completed survey conversation.
	// Only outcomes in the success class permit the verify transition.
	OutcomeSuccess StatusOutcome = "success"
	// OutcomeNoAnswer marks a call that timed out without an answer.
	OutcomeNoAnswer StatusOutcome = "no_answer"
	// OutcomeBusy marks a busy signal / SIT response.
	OutcomeBusy StatusOutcome = "busy"
	// OutcomeWrongPerson marks an answered call that did not reach the
	// intended respondent (relative, IVR, voicemail, ...).
	OutcomeWrongPerson StatusOutcome = "wrong_person"
	// OutcomeDNCHit marks a do-not-call list hit detected after dialing.
	OutcomeDNCHit StatusOutcome = "dnc_hit"
	// OutcomeTechFailure marks a telephony / network failure
	// (congestion, codec mismatch, ...).
	OutcomeTechFailure StatusOutcome = "tech_failure"
)

// Valid reports whether o is one of the recognized StatusOutcome enum
// values. The zero value ("") is NOT valid — callers that don't have an
// outcome yet must omit the field rather than send the empty string.
func (o StatusOutcome) Valid() bool {
	switch o {
	case OutcomeSuccess, OutcomeNoAnswer, OutcomeBusy,
		OutcomeWrongPerson, OutcomeDNCHit, OutcomeTechFailure:
		return true
	}
	return false
}

// IsSuccessClass reports whether o falls into the success class — the
// set of outcomes that permit the (status, go_verify) → verify
// transition. Today only OutcomeSuccess qualifies; the helper exists so
// future "answered but partial completion" outcomes can be folded in
// without scattering the predicate across the codebase.
func (o StatusOutcome) IsSuccessClass() bool {
	return o == OutcomeSuccess
}

// Snapshot is the immutable view of one operator's FSM state.
//
// Outcome carries the classified call result while State == StateStatus
// and is consulted by the (status, go_verify) → verify transition. It
// is zero-valued ("") in every other state. Transitions out of `status`
// reset it back to zero.
type Snapshot struct {
	TenantID       uuid.UUID
	OperatorID     uuid.UUID
	State          State
	StateEnteredAt time.Time
	ProjectID      *uuid.UUID
	CurrentCallID  *uuid.UUID
	RespondentID   *uuid.UUID
	PauseReason    *string
	HeartbeatAt    time.Time
	Outcome        StatusOutcome
}

// QueueItem is one row in the call queue. Priority+EnqueuedAt drives the
// ZSET score; AttemptN allows the retry orchestrator to apply per-attempt backoff.
type QueueItem struct {
	TenantID     uuid.UUID
	ProjectID    uuid.UUID
	RespondentID uuid.UUID
	Priority     uint8 // 0..9
	EnqueuedAt   time.Time
	AttemptN     uint8
	Phone        string // E.164
	Region       string // ISO 3166-2:RU code
}

// EnqueueRequest is the input for CallQueue.EnqueueRespondent.
type EnqueueRequest struct {
	TenantID     uuid.UUID
	ProjectID    uuid.UUID
	RespondentID uuid.UUID
	Phone        string
	Region       string
	Priority     uint8
	AttemptN     uint8
}

// StartShiftRequest is the input for OperatorFSM.StartShift.
type StartShiftRequest struct {
	TenantID   uuid.UUID
	OperatorID uuid.UUID
	ProjectID  uuid.UUID
	ClientIP   string
}

// GoPauseRequest is the input for OperatorFSM.GoPause.
type GoPauseRequest struct {
	TenantID   uuid.UUID
	OperatorID uuid.UUID
	Reason     string // bio_break | technical | training | ...
}

// CallStartedRequest is the input for OperatorFSM.RecordCallStarted.
type CallStartedRequest struct {
	TenantID     uuid.UUID
	OperatorID   uuid.UUID
	CallID       uuid.UUID
	RespondentID uuid.UUID
	StartedAt    time.Time
}

// CallEndedRequest is the input for OperatorFSM.RecordCallEnded.
//
// Outcome carries the classified result of the call attempt and is
// stored on the resulting `status` Snapshot. It gates the subsequent
// (status, go_verify) → verify transition: only success-class outcomes
// are allowed through (per CONTEXT.md). Callers MUST set Outcome to a
// valid value before invoking RecordCallEnded; an empty / unknown
// outcome makes the subsequent GoVerify call fail-loud with
// ErrInvalidTransition.
type CallEndedRequest struct {
	TenantID   uuid.UUID
	OperatorID uuid.UUID
	CallID     uuid.UUID
	EndedAt    time.Time
	Cause      string
	DurationMS int
	Outcome    StatusOutcome
}

// SubmitStatusRequest is the input for OperatorFSM.SubmitStatus.
type SubmitStatusRequest struct {
	TenantID     uuid.UUID
	OperatorID   uuid.UUID
	CallID       uuid.UUID
	RespondentID uuid.UUID
	Status       string
	Comment      string
}

// DialRequest is the input for Router.Dial. Translates into an OriginateCommand
// for the telephony bridge.
type DialRequest struct {
	CallID       uuid.UUID
	TenantID     uuid.UUID
	OperatorID   uuid.UUID
	RespondentID uuid.UUID
	ProjectID    uuid.UUID
	OperatorExt  string
	Phone        string
	FsNode       string
}

// ChannelEvent is the dialer-internal projection of a telephony event. The
// dialer subscribes to tenant.<t>.telephony.event.<call_id>.* and receives
// these events through Router.Subscribe.
type ChannelEvent struct {
	CallID   uuid.UUID
	Type     string // dialing | answered | hangup
	Cause    string
	Duration int // ms
	FsNode   string
}

// GenerateRequest is the input for RDDGenerator.Generate.
type GenerateRequest struct {
	TenantID  uuid.UUID
	ProjectID uuid.UUID
	N         int
	Quotas    map[string]int // region code → target count
	ABCRatio  float64        // share of АВС vs DEF in [0,1]
}

// GenerateResult is the return of RDDGenerator.Generate.
type GenerateResult struct {
	Generated     int
	ByRegion      map[string]int
	DuplicatesHit int
	DNCHit        int
	InvalidHit    int
	Throttled     bool
}

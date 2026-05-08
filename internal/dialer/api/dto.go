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

// Snapshot is the immutable view of one operator's FSM state.
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
type CallEndedRequest struct {
	TenantID   uuid.UUID
	OperatorID uuid.UUID
	CallID     uuid.UUID
	EndedAt    time.Time
	Cause      string
	DurationMS int
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

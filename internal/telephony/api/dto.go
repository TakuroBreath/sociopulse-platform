// Package api defines public contracts for the telephony module.
// Other modules import only from this package — never from telephony/{esl,pool,service}.
//
// telephony is the ESL ↔ NATS bridge. It owns the only ESL connections to
// FreeSWITCH nodes, exposes a NATS subject pair (cmd in / event out) that the
// dialer talks to, with idempotent command processing via Redis SETNX. It
// health-checks FS nodes every 5 seconds and publishes bridge.health
// heartbeats. It also serves the FreeSWITCH directory XML endpoint for
// SIP-WSS user provisioning.
package api

import (
	"time"

	"github.com/google/uuid"
)

// MixmonitorMode enumerates the listen-in modes supported by mod_dptools.
type MixmonitorMode string

const (
	// MMSilent records but does not let the listener be heard or speak.
	MMSilent MixmonitorMode = "silent"
	// MMRead lets the listener hear the operator side only.
	MMRead MixmonitorMode = "read"
	// MMWrite lets the listener whisper to the operator (operator hears, callee does not).
	MMWrite MixmonitorMode = "write"
	// MMBoth lets the listener barge into the conversation (full bridge).
	MMBoth MixmonitorMode = "both"
)

// ChannelEventType enumerates the FreeSWITCH channel events the bridge publishes.
type ChannelEventType string

const (
	EventDialing    ChannelEventType = "dialing"
	EventAnswer     ChannelEventType = "answer"
	EventBridge     ChannelEventType = "bridge"
	EventUnbridge   ChannelEventType = "unbridge"
	EventHangup     ChannelEventType = "hangup"
	EventDTMF       ChannelEventType = "dtmf"
	EventRecordStop ChannelEventType = "record_stop"
)

// RoutingStrategy enumerates the trunk-selection strategies the Router supports.
type RoutingStrategy string

const (
	RouteRoundRobin            RoutingStrategy = "round_robin"
	RouteWeighted              RoutingStrategy = "weighted"
	RouteLeastCost             RoutingStrategy = "least_cost"
	RouteLeastCostWithFallback RoutingStrategy = "least_cost_with_fallback"
)

// OriginateCommand asks FS to place an outbound call. CommandID is the
// idempotency key — Redis SETNX ensures replays are no-ops.
type OriginateCommand struct {
	CommandID      uuid.UUID // UUIDv7, idempotency key
	TenantID       uuid.UUID
	CallID         uuid.UUID
	OperatorExt    string // SIP user
	Number         string // E.164
	TrunkID        string // gateway name in mod_sofia
	FSNode         string
	PromptURL      string // optional consent prompt
	RecordingPath  string
	CallerID       string
	DialingTimeout time.Duration
}

// HangupCommand asks FS to end a call.
type HangupCommand struct {
	CommandID uuid.UUID
	CallID    uuid.UUID
	Cause     string // "NORMAL_CLEARING", "USER_BUSY", ...
}

// MixmonitorCommand starts a recording / listen-in stream.
type MixmonitorCommand struct {
	CommandID        uuid.UUID
	CallID           uuid.UUID
	Mode             MixmonitorMode
	ListenerEndpoint string // e.g. "user/lst_xxx"
}

// PlayCommand pushes an audio file URL to FS for playback into the call.
type PlayCommand struct {
	CommandID uuid.UUID
	CallID    uuid.UUID
	URL       string
}

// CreateUserCommand provisions a SIP user in the per-tenant FS directory.
type CreateUserCommand struct {
	CommandID   uuid.UUID
	TenantID    uuid.UUID
	SIPUser     string
	SIPPasswd   string // pre-hashed at boundary
	ContextHint string
}

// DeleteUserCommand removes a SIP user from the per-tenant FS directory.
type DeleteUserCommand struct {
	CommandID uuid.UUID
	TenantID  uuid.UUID
	SIPUser   string
}

// ChannelEvent is the canonical bridge → dialer event payload published on
// tenant.<t>.telephony.event.<call_id>.<event>.
type ChannelEvent struct {
	EventID     uuid.UUID
	TenantID    uuid.UUID
	CallID      uuid.UUID
	FSNode      string
	Type        ChannelEventType
	HangupCause string // populated when Type=hangup
	SIPResponse int    // populated when Type=hangup
	DurationMS  int64
	Timestamp   time.Time
	Headers     map[string]string
}

// SelectRequest is the input to Router.Select.
type SelectRequest struct {
	TenantID   uuid.UUID
	OperatorID uuid.UUID
	Region     string
	Strategy   RoutingStrategy
}

// SelectionResult is the return of Router.Select.
type SelectionResult struct {
	FSNode  string
	TrunkID string
	Reason  string // "primary" | "fallback:<trunk>" | "least-cost"
}

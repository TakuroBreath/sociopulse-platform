// Package api defines public contracts for the realtime module.
// Other modules import only from this package — never from realtime/service or realtime/store.
//
// realtime owns:
//   - WebSocket Hub (one Hub per cmd/api replica).
//   - Fan-out from NATS to local connections.
//   - Presence tracker (Redis-backed so cross-replica).
//   - Subscription RBAC matrix (which roles may subscribe to which topics).
//   - Listen-in service (silent for v1; whisper / barge stubbed for v2).
//   - Per-connection writer goroutine + backpressure (slow-consumer drop).
//   - Force-commands push channel.
package api

import (
	"encoding/json"
	"time"
)

// Topic enumerates the WebSocket channels a client may subscribe to.
type Topic string

const (
	// TopicOperatorsState carries operator FSM transitions (filter by ProjectID).
	TopicOperatorsState Topic = "operators.state"
	// TopicDialerQueue carries queue-depth notifications.
	TopicDialerQueue Topic = "dialer.queue"
	// TopicTrunksHealth carries trunk-health bridge events.
	TopicTrunksHealth Topic = "trunks.health"
	// TopicCallEvents carries per-call telephony events. Requires CallID filter.
	TopicCallEvents Topic = "call.events"
	// TopicNotifications is the per-user notification stream. Self-only.
	TopicNotifications Topic = "notifications.user"
	// TopicForceCommands is the server→client force-command channel. Self-only.
	TopicForceCommands Topic = "op.commands"
)

// FrameKind enumerates the WebSocket frame discriminator values.
type FrameKind string

const (
	FrameAuth         FrameKind = "auth"
	FrameAuthOK       FrameKind = "auth.ok"
	FrameAuthError    FrameKind = "auth.error"
	FrameRefresh      FrameKind = "refresh"
	FrameRefreshOK    FrameKind = "refresh.ok"
	FrameSubscribe    FrameKind = "subscribe"
	FrameSubscribeOK  FrameKind = "subscribe.ok"
	FrameSubscribeErr FrameKind = "subscribe.error"
	FrameUnsubscribe  FrameKind = "unsubscribe"
	FrameEvent        FrameKind = "event"
	FramePing         FrameKind = "ping"
	FramePong         FrameKind = "pong"
	FrameForce        FrameKind = "force.event"
)

// CloseReason is the WebSocket close code passed to Connection.Close.
// Numeric values follow RFC 6455 (1xxx) plus our custom 4xxx codes.
type CloseReason int

const (
	CloseNormal       CloseReason = 1000
	CloseGoingAway    CloseReason = 1001
	CloseProtocolErr  CloseReason = 1002
	CloseInvalidData  CloseReason = 1007
	ClosePolicyViol   CloseReason = 1008
	CloseUnauthorized CloseReason = 4401
	CloseRateLimited  CloseReason = 4429
)

// ListenMode enumerates the listen-in modes. Only Silent is enabled in v1.
type ListenMode string

const (
	ListenSilent  ListenMode = "silent"
	ListenWhisper ListenMode = "whisper" // v2
	ListenBarge   ListenMode = "barge"   // v2
)

// Claims is the realtime-local view of an authenticated user. It mirrors
// the auth.Claims fields the realtime layer actually uses; the WS auth
// frame validates a JWT via auth.Authenticator.ValidateAccessToken and
// then projects it into this shape.
type Claims struct {
	UserID   string
	TenantID string
	Roles    []string
}

// HubStats is the runtime counters surface for /metrics.
type HubStats struct {
	Connections    int
	BySubscription map[Topic]int
}

// BroadcastFilter narrows a Hub.Broadcast to a subset of connections.
// Empty fields mean "no filter on that dimension".
type BroadcastFilter struct {
	TenantID  string
	UserID    string
	ProjectID string
	CallID    string
}

// SubscriptionFilter narrows a Connection.Subscribe to a subset of events
// within a Topic. Different topics require different filters; the RBAC
// layer rejects subscriptions that lack the required filter.
type SubscriptionFilter struct {
	ProjectID  string
	OperatorID string
	CallID     string
}

// Subscription is one active subscription. ID is the server-side handle
// the client uses for Unsubscribe.
type Subscription struct {
	ID     string
	ConnID string
	UserID string
	Topic  Topic
	Filter SubscriptionFilter
}

// Frame is the canonical wire frame for WebSocket messages. The implementation
// JSON-encodes Frame for over-the-wire transmission; clients send the same shape.
type Frame struct {
	Type    FrameKind           `json:"type"`
	SubID   string              `json:"sub_id,omitempty"`
	Topic   Topic               `json:"topic,omitempty"`
	Filter  *SubscriptionFilter `json:"filter,omitempty"`
	Token   string              `json:"token,omitempty"`
	Payload json.RawMessage     `json:"payload,omitempty"`
	Reason  string              `json:"reason,omitempty"`
}

// StartListenRequest is the input for ListenInService.Start.
type StartListenRequest struct {
	Tenant     string
	ListenerID string
	CallID     string
	Mode       ListenMode
}

// ListenSession is the public projection of a listen-in session.
// SIPPassword is returned ONCE in the Start return; callers must persist
// it client-side (for the SIP-WSS bridge softphone) and never re-fetch.
type ListenSession struct {
	ID             string
	TenantID       string
	ListenerID     string
	CallID         string
	Mode           ListenMode
	SIPUser        string
	SIPPassword    string // returned ONCE
	VertoWSSURL    string
	StartedAt      time.Time
	StoppedAt      *time.Time
	FreeSwitchNode string
}

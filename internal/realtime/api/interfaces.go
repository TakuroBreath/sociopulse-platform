package api

import (
	"context"
	"encoding/json"
)

// WSConn is the abstract WebSocket connection the Hub speaks to. The real
// adapter wraps gorilla/websocket; tests inject an in-memory implementation.
type WSConn interface {
	// ReadFrame reads one frame's bytes (the Hub deserialises into Frame).
	ReadFrame(ctx context.Context) (data []byte, err error)
	// WriteFrame writes one frame's bytes.
	WriteFrame(ctx context.Context, data []byte) error
	// Close shuts down the connection with a code and reason string.
	Close(code CloseReason, reason string) error
	// RemoteAddr returns the remote IP:port for logging.
	RemoteAddr() string
}

// Hub is the per-replica WebSocket fan-out core. Exactly one Hub is
// constructed in cmd/api per process.
type Hub interface {
	// Connect attaches a fresh WSConn (post-auth) to the Hub. Returns the
	// Connection handle.
	Connect(ctx context.Context, conn WSConn, claims Claims) (Connection, error)
	// Broadcast pushes payload to every connection matching filter on topic.
	// Returns the number of recipients reached.
	Broadcast(ctx context.Context, topic Topic, payload json.RawMessage, filter BroadcastFilter) int
	// DisconnectByUser closes every connection for the user (used by force-logout-all).
	DisconnectByUser(ctx context.Context, tenantID, userID string)
	// Stats returns runtime counters.
	Stats() HubStats
}

// Connection is the per-WSConn handle the Hub returns from Connect.
type Connection interface {
	// ID returns the server-side connection ID (logged at every state change).
	ID() string
	// Claims returns the authenticated identity tied to this connection.
	Claims() Claims
	// Subscribe registers a subscription on this connection.
	Subscribe(topic Topic, filter SubscriptionFilter) (subID string, err error)
	// Unsubscribe removes the subscription with the given ID.
	Unsubscribe(subID string)
	// Close terminates the connection with the given reason.
	Close(reason CloseReason)
}

// PresenceTracker is the Redis-backed cross-replica presence map.
type PresenceTracker interface {
	// OnConnect marks the (tenant, user) presence as online and records the replica.
	OnConnect(ctx context.Context, tenantID, userID, replicaID string) error
	// OnDisconnect marks the (tenant, user) presence as offline.
	OnDisconnect(ctx context.Context, tenantID, userID string) error
	// Touch refreshes the per-user TTL so the entry doesn't expire mid-session.
	Touch(ctx context.Context, tenantID, userID string) error
	// IsOnline returns whether the user has at least one active connection.
	IsOnline(ctx context.Context, tenantID, userID string) (bool, error)
	// OnlineUsers returns every user ID currently online for the tenant.
	OnlineUsers(ctx context.Context, tenantID string) ([]string, error)
}

// ListenInService brokers supervisor listen-in sessions. v1 supports only
// the silent mode (recording-channel mirror); whisper / barge are stubbed for v2.
type ListenInService interface {
	// Start spins up a listen session and returns the SIP creds for the supervisor's softphone.
	Start(ctx context.Context, in StartListenRequest) (*ListenSession, error)
	// Stop ends the session.
	Stop(ctx context.Context, sessionID string) error
	// List returns the active sessions for the tenant.
	List(ctx context.Context, tenantID string) ([]*ListenSession, error)
}

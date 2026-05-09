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

// ResolvedTenant is the projection of an entity (user, project, call)
// that resolver ports return. The TenantID field is the cross-check
// target: TopicRBAC.Allow rejects a subscription when the resolved
// TenantID does not match the subscriber's claims.TenantID.
type ResolvedTenant struct {
	// TenantID is the entity's owning tenant. Returned as a string to
	// match the existing realtime/api convention (Claims.TenantID is a
	// string; Hub.Broadcast filters use string TenantID).
	TenantID string
}

// UserResolver maps a user_id to its tenant. Used by TopicRBAC.Allow
// to reject `notifications.user` / `op.commands` subscriptions whose
// filter.OperatorID belongs to a different tenant than the
// subscriber's claims.
//
// Implementations MUST return an error (ErrCrossTenantSubscribe-folded
// at the RBAC layer) when the user does not exist; TopicRBAC treats
// not-found the same as cross-tenant — both are a "you cannot
// subscribe" signal — so the wire response is identical and the
// client can't probe user existence cross-tenant.
type UserResolver interface {
	// Get resolves user_id to its owning tenant. Returns an error
	// when the user is not resolvable. ctx-aware so the realtime
	// layer can bound the resolve under its handshake/subscribe
	// deadline.
	Get(ctx context.Context, userID string) (ResolvedTenant, error)
}

// ProjectResolver maps a project_id to its tenant. Used by
// TopicRBAC.Allow to reject `operators.state` subscriptions whose
// filter.ProjectID belongs to a different tenant.
//
// Same not-found semantics as UserResolver — the realtime layer
// folds not-found into cross-tenant rejection.
type ProjectResolver interface {
	// Get resolves project_id to its owning tenant. Returns an
	// error when the project is not resolvable.
	Get(ctx context.Context, projectID string) (ResolvedTenant, error)
}

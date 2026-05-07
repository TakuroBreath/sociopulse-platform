// Package api defines public contracts for the audit module.
// Other modules import only from this package — never from audit/service or audit/store.
//
// audit is the trunk-most leaf module — it has no internal dependencies.
// Every other module calls Logger.Write after a state-changing action;
// the underlying implementation persists rows to the audit_log table and
// supports a weekly archive pass that moves rows older than one year to
// S3 cold tier (FR-K1, FR-K4).
package api

import (
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// ActorKind enumerates who initiated the audited action.
type ActorKind string

const (
	// ActorUser indicates the action was initiated by an authenticated user.
	ActorUser ActorKind = "user"
	// ActorSystem indicates the action was initiated by a background process or scheduled job.
	ActorSystem ActorKind = "system"
	// ActorServiceOwner indicates the action was initiated by a Service-Owner — a cross-tenant operator.
	ActorServiceOwner ActorKind = "service-owner"
)

// Event is one row in the audit log. Payload is jsonb-encoded server-side
// after redaction patterns are stripped. Field tags reflect the canonical
// JSON encoding used both for the audit_log.payload column and the NATS
// audit.event subject.
type Event struct {
	ID        uuid.UUID      `json:"id"`
	TenantID  uuid.UUID      `json:"tenant_id"`          // optional: cross-tenant Service-Owner events leave it zero
	ActorID   *uuid.UUID     `json:"actor_id,omitempty"` // user_id or nil for system actions
	ActorKind ActorKind      `json:"actor_kind"`         // user | system | service-owner
	Action    string         `json:"action"`             // e.g. "auth.login", "recording.accessed"
	Target    string         `json:"target"`             // resource pointer ("call:<id>", "user:<id>")
	Payload   map[string]any `json:"payload,omitempty"`  // jsonb, redacted
	IP        netip.Addr     `json:"ip,omitzero"`
	UserAgent string         `json:"user_agent,omitempty"`
	Timestamp time.Time      `json:"ts"`
}

// ListFilter narrows a Reader.List query. Cursor is opaque (timestamp + id encoded).
type ListFilter struct {
	TenantID uuid.UUID
	Action   string // exact match if non-empty
	From, To time.Time
	ActorID  *uuid.UUID
	Cursor   string // opaque (timestamp + id encoded)
	Limit    int    // 1..500, default 100
}

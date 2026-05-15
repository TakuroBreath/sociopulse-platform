// call_tenant_resolver.go declares the narrow port the
// pkg/middleware/tenant.RequireSameTenant guard consumes to gate
// /api/calls/:id/hangup against cross-tenant access. Plan 21 Task 3
// closes the Plan 13.2.5 out-of-scope finding tracked for v0.0.26.
//
// The port is intentionally separate from Router / OperatorFSM because:
//
//   - The lookup is cross-tenant by definition (the middleware does
//     not yet know the calling tenant matches; that's what we're
//     trying to verify). Implementations MUST use pool.BypassRLS so
//     the read sees rows regardless of any prior SET LOCAL
//     app.tenant_id on the connection.
//   - Keeps the cross-tenant probe off Router's public surface so
//     existing consumers (dialer/transport/nats, /api/calls/:id/status)
//     need no changes.
//
// Mirror of Plan 11.4's recording.CallTenantLookup shape, but reads
// the calls table (where the row exists at hangup time) rather than
// call_recordings (which is only populated after recording-uploader
// finalises the audio object).
package api

import (
	"context"

	"github.com/google/uuid"
)

// CallTenantResolver resolves a call_id to its owning tenant via a
// BypassRLS SELECT against the calls table. Used by cmd/api's dialer
// transport to populate the tenant.RequireSameTenant middleware on
// POST /api/calls/:id/hangup (Plan 21 Task 3).
//
// Implementations MUST return ErrCallNotFound when the call_id has no
// row in calls. The transport-layer middleware adapter folds this
// sentinel into pkg/middleware/tenant.ErrNotFound so the wire response
// is a 404 with no body — indistinguishable from a "wrong tenant"
// mismatch, defeating existence-probe enumeration.
//
// Any other error is wrapped via fmt.Errorf("ctx: %w", err) and
// surfaces as HTTP 500 — a transient storage hiccup must not silently
// downgrade the caller's safety guarantee to a 404 that lets the
// request pass through.
type CallTenantResolver interface {
	// LookupCallTenant resolves call_id to its owning tenant. ctx-aware
	// so the middleware can bound the lookup under the per-request
	// deadline.
	LookupCallTenant(ctx context.Context, callID uuid.UUID) (tenantID uuid.UUID, err error)
}

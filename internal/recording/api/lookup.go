// Package api — lookup.go declares the narrow CallTenantLookup port the
// realtime CallResolver consumes. The port is intentionally separate
// from RecordingService because:
//
//   - RecordingService.Get requires (tenantID, callID) at the boundary;
//     CallTenantLookup takes only callID and resolves the tenant via
//     BypassRLS — the cross-tenant case the realtime resolver needs.
//   - Keeps the new lookup off the public RecordingService surface so
//     existing consumers (gRPC commit pipeline, HTTP playback, retention
//     worker) need no changes.
//
// Plan 11.4 Task 4.
package api

import (
	"context"

	"github.com/google/uuid"
)

// CallTenantLookup resolves a call_id to its owning tenant via a
// BypassRLS SELECT against call_recordings. Used by cmd/api's
// callResolverAdapter (Plan 11.4 Task 7) to populate the realtime
// CallResolver port.
//
// Implementations MUST return the existing store-level sentinel
// (internal/recording/store.ErrCallNotFound) wrapped via fmt.Errorf
// when the call_id has no row in call_recordings. The realtime layer
// folds not-found into ErrCrossTenantSubscribe so the wire response
// is identical and clients cannot probe call existence cross-tenant.
type CallTenantLookup interface {
	// LookupTenant resolves call_id to its owning tenant. ctx-aware so
	// the realtime layer can bound the lookup under the subscribe
	// deadline (5s inner timeout via CachedCallResolver).
	LookupTenant(ctx context.Context, callID uuid.UUID) (tenantID uuid.UUID, err error)
}

package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/modules"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// locatorRecordingCallTenantLookup mirrors the recording module's
// publication key for the new CallTenantLookup port. cmd/api looks up
// this key to build the realtime CallResolver adapter; recording.Module
// publishes under the same name. Plan 11.4 Task 7.
const locatorRecordingCallTenantLookup = "recording.CallTenantLookup"

// callResolverAdapter projects rapi.CallTenantLookup onto
// rtapi.CallResolver. The wire-string call_id is parsed via uuid.Parse —
// a malformed UUID surfaces as a wrapped error that TopicRBAC.Allow
// folds into ErrCrossTenantSubscribe (security: client cannot probe
// call existence cross-tenant).
//
// Mirrors userResolverAdapter / projectResolverAdapter from realtime.go.
type callResolverAdapter struct {
	lookup rapi.CallTenantLookup
}

// newCallResolverAdapter wraps a rapi.CallTenantLookup. nil lookup
// panics — the wiring bug surfaces at cmd/api boot rather than first
// subscribe.
func newCallResolverAdapter(lookup rapi.CallTenantLookup) *callResolverAdapter {
	if lookup == nil {
		panic("cmd/api: newCallResolverAdapter: lookup must be non-nil")
	}
	return &callResolverAdapter{lookup: lookup}
}

// Get implements rtapi.CallResolver.
func (a *callResolverAdapter) Get(ctx context.Context, callID string) (rtapi.ResolvedTenant, error) {
	id, err := uuid.Parse(callID)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: parse call_id %q: %w", callID, err)
	}
	tenantID, err := a.lookup.LookupTenant(ctx, id)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: lookup tenant for call %s: %w", id, err)
	}
	return rtapi.ResolvedTenant{TenantID: tenantID.String()}, nil
}

// Compile-time interface check.
var _ rtapi.CallResolver = (*callResolverAdapter)(nil)

// registerCallResolver looks up recording.CallTenantLookup in the
// locator and registers the rtapi.CallResolver adapter under
// LocatorCallResolver. Mirrors registerUserResolver /
// registerProjectResolver.
//
// Order matters: this MUST run AFTER recording.Module.Register (which
// publishes recording.CallTenantLookup) AND BEFORE realtime.Module.Register
// (which looks up rtapi.LocatorCallResolver). Missing-but-tolerated
// paths log INFO and skip the registration; type-mismatched entries
// log WARN and skip. Either way the boot does not abort.
func registerCallResolver(locator modules.ServiceLocator, logger *zap.Logger) {
	if locator == nil {
		logger.Info("realtime resolvers: locator missing, skipping call resolver registration")
		return
	}
	v, ok := locator.Lookup(locatorRecordingCallTenantLookup)
	if !ok {
		logger.Info("realtime resolvers: recording.CallTenantLookup missing; CallResolver disabled (degraded boot)")
		return
	}
	lookup, ok := v.(rapi.CallTenantLookup)
	if !ok {
		logger.Warn("realtime resolvers: recording.CallTenantLookup registered with wrong type; CallResolver disabled",
			zap.String("got_type", fmt.Sprintf("%T", v)),
		)
		return
	}
	locator.Register(rtapi.LocatorCallResolver, rtapi.CallResolver(newCallResolverAdapter(lookup)))
	logger.Info("realtime resolvers: CallResolver registered from recording.CallTenantLookup")
}

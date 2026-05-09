package main

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/modules"
	rtevents "github.com/sociopulse/platform/internal/realtime/events"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

// locatorTenantService mirrors the same key the tenancy module
// publishes under (internal/tenancy/module.go). Kept private here
// because cmd/api is the only consumer that needs to look it up; we
// avoid coupling the tenancy api/ package to a publicly exported
// constant for one caller.
const locatorTenantService = "tenancy.TenantService"

// tenancyTenantLister adapts tenancy.TenantService → events.TenantLister.
//
// The realtime *TrunksReplicator only needs the projected list of active
// tenant UUIDs (one per Hub.Broadcast fan-out). The tenancy API exposes
// the richer Tenant DTO via List; the adapter trims the projection here
// so the events package stays free of tenancy DTO imports (Plan 11.1
// scope rule: events/ MUST NOT import tenancy/).
//
// Caching policy lives on the lister side per Plan 11.1 Task 2 — the
// replicator does not cache. The tenancy.TenantService implementation
// chooses whether to memoize List(active) results; this adapter is a
// thin pass-through.
type tenancyTenantLister struct {
	svc tenancyapi.TenantService
}

// newTenancyTenantLister wraps a tenancy.TenantService.
func newTenancyTenantLister(svc tenancyapi.TenantService) *tenancyTenantLister {
	return &tenancyTenantLister{svc: svc}
}

// ListActiveTenantIDs returns the UUID-string of every active tenant.
//
// Implementation detail: the tenancy filter accepts a *TenantStatus, so
// we pin it to TenantStatusActive. The Limit field defaults to 50 in
// the service layer; we explicitly pass 500 (the documented maximum)
// so a tenancy with hundreds of tenants is still fanned out to in a
// single trunks.health event. Pagination beyond 500 active tenants is
// out-of-scope for v1 — callers will see Plan 11.1 Task 2's metric
// `realtime_dispatcher_dispatch_failures_total{reason="tenant_lister_failed"}`
// jump if List ever errors at that ceiling.
func (a *tenancyTenantLister) ListActiveTenantIDs(ctx context.Context) ([]string, error) {
	active := tenancyapi.TenantStatusActive
	tenants, err := a.svc.List(ctx, tenancyapi.ListTenantsFilter{
		Status: &active,
		Limit:  500, //nolint:mnd // documented maximum on tenancy.ListTenantsFilter; v1 ceiling
	})
	if err != nil {
		return nil, fmt.Errorf("cmd/api: list active tenants: %w", err)
	}
	out := make([]string, 0, len(tenants))
	for _, t := range tenants {
		out = append(out, t.ID.String())
	}
	return out, nil
}

// emptyTenantLister is the fallback used when the tenancy module is
// missing from the locator (Redis-less / minimal-boot test). It always
// returns no tenants so *TrunksReplicator turns trunks.health into a
// metrics-only no-op rather than panicking on a nil dependency.
type emptyTenantLister struct{}

// ListActiveTenantIDs returns an empty slice and a nil error.
func (emptyTenantLister) ListActiveTenantIDs(_ context.Context) ([]string, error) {
	return nil, nil
}

// resolveTenantLister picks the production tenancy adapter when the
// tenancy module has registered its TenantService; otherwise it
// returns the empty fallback and logs an INFO line so operators know
// trunks.health fan-out is degraded. The latter case is reachable in
// the minimal-boot path (no Postgres → tenancy module skipped) and in
// integration tests that don't wire the tenancy module.
func resolveTenantLister(locator modules.ServiceLocator, logger *zap.Logger) rtevents.TenantLister {
	if locator == nil {
		logger.Info("realtime trunks replicator: locator missing, using empty TenantLister fallback")
		return emptyTenantLister{}
	}
	v, ok := locator.Lookup(locatorTenantService)
	if !ok {
		logger.Info("realtime trunks replicator: tenancy.TenantService missing from locator, using empty TenantLister fallback")
		return emptyTenantLister{}
	}
	svc, ok := v.(tenancyapi.TenantService)
	if !ok {
		logger.Warn("realtime trunks replicator: tenancy.TenantService registered with wrong type, using empty TenantLister fallback",
			zap.String("got_type", fmt.Sprintf("%T", v)),
		)
		return emptyTenantLister{}
	}
	return newTenancyTenantLister(svc)
}

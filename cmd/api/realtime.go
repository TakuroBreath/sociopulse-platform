package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/modules"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	rtevents "github.com/sociopulse/platform/internal/realtime/events"
	service "github.com/sociopulse/platform/internal/realtime/service"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

// locatorAuthUserService and locatorCRMProjectService mirror the keys
// the auth and crm modules register their public services under. Kept
// private here because cmd/api is the only caller wiring the realtime
// resolvers; mirroring the keys avoids a transitive import of
// internal/auth/module.go (which pulls the entire auth/service stack
// into cmd/api at compile time when only the api/ DTO is needed).
const (
	locatorAuthUserService   = "auth.UserService"
	locatorCRMProjectService = "crm.ProjectService"
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

// authUserGetter is the narrow surface cmd/api needs from
// auth.UserService to satisfy realtime.UserResolver. The auth module
// exposes a richer interface; this slice is enough to project a user
// onto its tenant for the realtime cross-tenant check.
//
// auth.UserService.Get is tenant-agnostic by design (it opens a
// BypassRLS tx so admin tooling can resolve a user id to its tenant
// before any per-tenant flow); that property is exactly what the
// realtime resolver needs — the subscriber's claims tenant is used as
// the "wanted" tenant and Get must return the row regardless of which
// tenant owns it. See internal/auth/service/user_service.go::Get for
// the implementation.
type authUserGetter interface {
	Get(ctx context.Context, userID uuid.UUID) (authapi.User, error)
}

// userResolverAdapter projects auth.UserService onto rtapi.UserResolver.
// The wire-string user_id is parsed via uuid.Parse — a malformed UUID
// surfaces as a wrapped error that TopicRBAC.Allow folds into
// ErrCrossTenantSubscribe (security: client cannot probe entity
// existence cross-tenant).
type userResolverAdapter struct {
	svc authUserGetter
}

// newUserResolverAdapter wraps an authUserGetter. nil svc panics —
// the wiring bug surfaces at cmd/api boot rather than first subscribe.
func newUserResolverAdapter(svc authUserGetter) *userResolverAdapter {
	if svc == nil {
		panic("cmd/api: newUserResolverAdapter: svc must be non-nil")
	}
	return &userResolverAdapter{svc: svc}
}

// Get implements rtapi.UserResolver.
func (a *userResolverAdapter) Get(ctx context.Context, userID string) (rtapi.ResolvedTenant, error) {
	id, err := uuid.Parse(userID)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: parse user_id %q: %w", userID, err)
	}
	user, err := a.svc.Get(ctx, id)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: get user %s: %w", id, err)
	}
	return rtapi.ResolvedTenant{TenantID: user.TenantID.String()}, nil
}

// crmProjectGetter is the tenant-agnostic lookup the resolver uses.
// crm.ProjectService.Get opens a BypassRLS tx for the same reason as
// auth.UserService.Get — admin tooling routinely resolves a project
// id to its tenant before a per-tenant flow. See
// internal/crm/service/project_service.go::Get.
type crmProjectGetter interface {
	Get(ctx context.Context, projectID uuid.UUID) (*crmapi.Project, error)
}

// projectResolverAdapter mirrors userResolverAdapter for project IDs.
type projectResolverAdapter struct {
	svc              crmProjectGetter
	bumpInconsistent func(adapterType string)
}

// newProjectResolverAdapterWithMetrics is the metric-aware variant
// used by registerProjectResolver (Plan 11.3 Task 4 wiring). Tests
// use this directly to inject a counting fake callback. nil
// bumpInconsistent is replaced with a no-op so degraded boot
// (no service.Metrics in the locator) doesn't NPE.
func newProjectResolverAdapterWithMetrics(
	svc crmProjectGetter,
	bumpInconsistent func(adapterType string),
) *projectResolverAdapter {
	if svc == nil {
		panic("cmd/api: newProjectResolverAdapterWithMetrics: svc must be non-nil")
	}
	if bumpInconsistent == nil {
		bumpInconsistent = func(string) {} // nil-safe: no-op metric callback
	}
	return &projectResolverAdapter{
		svc:              svc,
		bumpInconsistent: bumpInconsistent,
	}
}

// Get implements rtapi.ProjectResolver.
func (a *projectResolverAdapter) Get(ctx context.Context, projectID string) (rtapi.ResolvedTenant, error) {
	id, err := uuid.Parse(projectID)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: parse project_id %q: %w", projectID, err)
	}
	proj, err := a.svc.Get(ctx, id)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: get project %s: %w", id, err)
	}
	if proj == nil {
		// Defensive: ProjectService.Get returns (nil, ErrProjectNotFound)
		// on miss; we handle the error path above. A nil-without-error
		// would be a service-layer bug — surface it via metric so the
		// regression doesn't hide as a legitimate cross-tenant
		// rejection. Plan 11.3 Task 4.
		a.bumpInconsistent("project")
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: get project %s: nil project returned without error", id)
	}
	return rtapi.ResolvedTenant{TenantID: proj.TenantID.String()}, nil
}

// Compile-time interface checks for the resolver adapters. Mirrors
// the pattern used by noopPublisher / noopSubscriber in eventbus.go;
// surfaces a port-signature drift here at cmd/api rather than far
// away at the realtime.Module.Register call site (Plan 11.2 Task 5
// review NIT M-1).
var (
	_ rtapi.UserResolver    = (*userResolverAdapter)(nil)
	_ rtapi.ProjectResolver = (*projectResolverAdapter)(nil)
)

// registerRealtimeResolvers wires the realtime cross-tenant resolvers
// into the locator BEFORE realtime.Module.Register runs. The realtime
// module looks up rtapi.LocatorUserResolver / LocatorProjectResolver
// and falls back to empty resolvers (which reject every cross-tenant
// lookup) when an entry is missing — degraded boot is still safe.
//
// Order matters: this MUST run AFTER auth.Module.Register +
// crm.Module.Register (they populate auth.UserService /
// crm.ProjectService) AND BEFORE realtime.Module.Register (which
// looks up the resolver keys).
//
// Missing-but-tolerated paths log INFO and skip the registration. A
// type-mismatched entry (a wiring bug — somebody registered the key
// with the wrong type) logs WARN and skips; the realtime module's
// empty fallback path covers it. Either way the boot does not abort.
func registerRealtimeResolvers(locator modules.ServiceLocator, logger *zap.Logger) {
	if locator == nil {
		logger.Info("realtime resolvers: locator missing, skipping resolver registration")
		return
	}
	registerUserResolver(locator, logger)
	registerProjectResolver(locator, logger)
}

// registerUserResolver looks up auth.UserService and registers the
// rtapi.UserResolver adapter. Pulled out of registerRealtimeResolvers
// to keep the two dimensions parallel (gocognit-friendly).
func registerUserResolver(locator modules.ServiceLocator, logger *zap.Logger) {
	v, ok := locator.Lookup(locatorAuthUserService)
	if !ok {
		logger.Info("realtime resolvers: auth.UserService missing; UserResolver disabled (degraded boot)")
		return
	}
	svc, ok := v.(authapi.UserService)
	if !ok {
		logger.Warn("realtime resolvers: auth.UserService registered with wrong type; UserResolver disabled",
			zap.String("got_type", fmt.Sprintf("%T", v)),
		)
		return
	}
	locator.Register(rtapi.LocatorUserResolver, rtapi.UserResolver(newUserResolverAdapter(svc)))
	logger.Info("realtime resolvers: UserResolver registered from auth.UserService")
}

// registerProjectResolver mirrors registerUserResolver for the project
// dimension.
func registerProjectResolver(locator modules.ServiceLocator, logger *zap.Logger) {
	v, ok := locator.Lookup(locatorCRMProjectService)
	if !ok {
		logger.Info("realtime resolvers: crm.ProjectService missing; ProjectResolver disabled (degraded boot)")
		return
	}
	svc, ok := v.(crmapi.ProjectService)
	if !ok {
		logger.Warn("realtime resolvers: crm.ProjectService registered with wrong type; ProjectResolver disabled",
			zap.String("got_type", fmt.Sprintf("%T", v)),
		)
		return
	}
	// Plan 11.3 Task 4: thread realtime.ConnectionMetrics through to
	// the adapter so the (nil, nil) defensive branch lands on
	// realtime_resolver_adapter_inconsistent_total{adapter_type="project"}.
	// Missing metrics → no-op (degraded boot tolerance).
	var bump func(string)
	if metricsRaw, ok := locator.Lookup(rtapi.LocatorConnectionMetrics); ok {
		if m, ok := metricsRaw.(*service.Metrics); ok {
			bump = m.ObserveResolverAdapterInconsistent
		}
	}
	locator.Register(rtapi.LocatorProjectResolver,
		rtapi.ProjectResolver(newProjectResolverAdapterWithMetrics(svc, bump)))
	logger.Info("realtime resolvers: ProjectResolver registered from crm.ProjectService")
}

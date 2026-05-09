// resolver_lookup.go provides the locator-based lookup of
// rtapi.UserResolver + rtapi.ProjectResolver populated by cmd/api
// (Plan 11.2 Task 5). The realtime module itself cannot import
// internal/auth/* or internal/crm/* (scope rule from Plan 11.1
// Task 2), so cmd/api adapts auth.UserService + crm.ProjectService to
// the rtapi.UserResolver/ProjectResolver shape and registers them
// under rtapi.LocatorUserResolver / rtapi.LocatorProjectResolver
// keys. This file performs the lookup and falls back to empty
// resolvers when the production adapters aren't wired.
//
// The empty resolvers REJECT every cross-tenant lookup with
// rtapi.ErrCrossTenantSubscribe — strictly safer than no check.
// In practice this means a degraded boot (no auth/crm modules)
// produces RBAC behaviour equivalent to "accept role-correct
// subscribes; reject any with a non-empty filter UUID".
package realtime

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/modules"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// resolveResolversFromLocator looks up the rtapi.UserResolver +
// rtapi.ProjectResolver registered by cmd/api under the
// rtapi.LocatorUserResolver / LocatorProjectResolver keys. Missing
// entries fall back to empty resolvers that reject every cross-tenant
// lookup.
//
// The function is defensive on its inputs: a nil logger is replaced
// with zap.NewNop and a nil locator returns the empty fallbacks
// without panicking. This lets test setups that don't bother
// constructing a logger or locator still call the helper directly.
func resolveResolversFromLocator(locator modules.ServiceLocator, logger *zap.Logger) (rtapi.UserResolver, rtapi.ProjectResolver) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if locator == nil {
		logger.Info("realtime resolvers: locator missing, using empty fallbacks")
		return emptyUserResolver{}, emptyProjectResolver{}
	}
	users := lookupUserResolver(locator, logger)
	projects := lookupProjectResolver(locator, logger)
	return users, projects
}

// lookupUserResolver does the actual locator lookup with type-safety
// + empty fallback. Pulled out of resolveResolversFromLocator so the
// two dimensions stay parallel (gocognit-friendly).
func lookupUserResolver(locator modules.ServiceLocator, logger *zap.Logger) rtapi.UserResolver {
	v, ok := locator.Lookup(rtapi.LocatorUserResolver)
	if !ok {
		logger.Info("realtime resolvers: UserResolver missing, using empty fallback")
		return emptyUserResolver{}
	}
	r, ok := v.(rtapi.UserResolver)
	if !ok {
		logger.Warn("realtime resolvers: UserResolver registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", v)),
		)
		return emptyUserResolver{}
	}
	return r
}

// lookupProjectResolver mirrors lookupUserResolver for the project port.
func lookupProjectResolver(locator modules.ServiceLocator, logger *zap.Logger) rtapi.ProjectResolver {
	v, ok := locator.Lookup(rtapi.LocatorProjectResolver)
	if !ok {
		logger.Info("realtime resolvers: ProjectResolver missing, using empty fallback")
		return emptyProjectResolver{}
	}
	r, ok := v.(rtapi.ProjectResolver)
	if !ok {
		logger.Warn("realtime resolvers: ProjectResolver registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", v)),
		)
		return emptyProjectResolver{}
	}
	return r
}

// emptyUserResolver rejects every lookup with ErrCrossTenantSubscribe.
// Used when cmd/api hasn't wired the production adapter (degraded
// boot, integration test without auth module, etc.). Strictly safer
// than no check — TopicRBAC.Allow surfaces every cross-tenant
// candidate as denied rather than silently accepting.
type emptyUserResolver struct{}

// Get returns ErrCrossTenantSubscribe for every input.
func (emptyUserResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, rtapi.ErrCrossTenantSubscribe
}

// emptyProjectResolver mirrors emptyUserResolver for the project port.
type emptyProjectResolver struct{}

// Get returns ErrCrossTenantSubscribe for every input.
func (emptyProjectResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, rtapi.ErrCrossTenantSubscribe
}

// Compile-time interface checks. Keeping these next to the
// implementations means a port signature change breaks the build at
// the empty fallback, not far away in the consumer.
var (
	_ rtapi.UserResolver    = emptyUserResolver{}
	_ rtapi.ProjectResolver = emptyProjectResolver{}
)

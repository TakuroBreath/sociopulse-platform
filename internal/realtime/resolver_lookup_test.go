// resolver_lookup_test.go covers the locator-based lookup of
// rtapi.UserResolver + rtapi.ProjectResolver and the empty-fallback
// path for degraded boot (no auth/crm modules). The test is in
// `package realtime` (rather than `realtime_test`) so it can call
// the package-private resolveResolversFromLocator helper directly;
// the production-facing module_test.go uses `realtime_test` and
// drives the full Module.Register surface.
package realtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/modules"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// stubUserResolver and stubProjectResolver are tiny test doubles —
// the real adapters live in cmd/api and are tested via cmd/api's
// own tests.
type stubUserResolver struct{ tenantID string }

func (s stubUserResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{TenantID: s.tenantID}, nil
}

type stubProjectResolver struct{ tenantID string }

func (s stubProjectResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{TenantID: s.tenantID}, nil
}

// TestResolveResolversFromLocator_LookupSuccess verifies the locator
// path returns the registered resolvers when both keys are present.
func TestResolveResolversFromLocator_LookupSuccess(t *testing.T) {
	t.Parallel()

	loc := modules.NewMapLocator()
	loc.Register(rtapi.LocatorUserResolver, rtapi.UserResolver(stubUserResolver{tenantID: "t-A"}))
	loc.Register(rtapi.LocatorProjectResolver, rtapi.ProjectResolver(stubProjectResolver{tenantID: "t-B"}))

	users, projects := resolveResolversFromLocator(loc, zap.NewNop())

	got, err := users.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.Equal(t, "t-A", got.TenantID)

	got, err = projects.Get(t.Context(), "p1")
	require.NoError(t, err)
	assert.Equal(t, "t-B", got.TenantID)
}

// TestResolveResolversFromLocator_MissingFallsBackToEmpty verifies
// that a degraded boot (resolver entries absent) gets the empty
// fallback that REJECTS every lookup with ErrCrossTenantSubscribe.
func TestResolveResolversFromLocator_MissingFallsBackToEmpty(t *testing.T) {
	t.Parallel()

	loc := modules.NewMapLocator() // empty

	users, projects := resolveResolversFromLocator(loc, zap.NewNop())

	_, err := users.Get(t.Context(), "u1")
	require.ErrorIs(t, err, rtapi.ErrCrossTenantSubscribe,
		"empty UserResolver fallback must reject all lookups")

	_, err = projects.Get(t.Context(), "p1")
	require.ErrorIs(t, err, rtapi.ErrCrossTenantSubscribe,
		"empty ProjectResolver fallback must reject all lookups")
}

// TestResolveResolversFromLocator_NilLocator covers the boot-time
// guard: a nil locator (impossible in production but defensive)
// gracefully degrades to empty fallbacks.
func TestResolveResolversFromLocator_NilLocator(t *testing.T) {
	t.Parallel()

	users, projects := resolveResolversFromLocator(nil, zap.NewNop())

	_, err := users.Get(t.Context(), "u1")
	require.ErrorIs(t, err, rtapi.ErrCrossTenantSubscribe)

	_, err = projects.Get(t.Context(), "p1")
	require.ErrorIs(t, err, rtapi.ErrCrossTenantSubscribe)
}

// TestResolveResolversFromLocator_WrongTypeFallsBackToEmpty verifies
// a locator entry registered with the wrong type (a wiring bug) does
// NOT panic; it logs a Warn and falls back to empty.
func TestResolveResolversFromLocator_WrongTypeFallsBackToEmpty(t *testing.T) {
	t.Parallel()

	loc := modules.NewMapLocator()
	loc.Register(rtapi.LocatorUserResolver, "not a resolver")
	loc.Register(rtapi.LocatorProjectResolver, 42)

	users, projects := resolveResolversFromLocator(loc, zap.NewNop())

	_, err := users.Get(t.Context(), "u1")
	require.ErrorIs(t, err, rtapi.ErrCrossTenantSubscribe)

	_, err = projects.Get(t.Context(), "p1")
	require.ErrorIs(t, err, rtapi.ErrCrossTenantSubscribe)
}

// TestResolveResolversFromLocator_NilLogger covers the defensive nil
// logger path: a nil zap.Logger is replaced internally with a Nop so
// the helper does not panic on the first log call.
func TestResolveResolversFromLocator_NilLogger(t *testing.T) {
	t.Parallel()

	loc := modules.NewMapLocator()

	users, projects := resolveResolversFromLocator(loc, nil)

	_, err := users.Get(t.Context(), "u1")
	require.ErrorIs(t, err, rtapi.ErrCrossTenantSubscribe)

	_, err = projects.Get(t.Context(), "p1")
	require.ErrorIs(t, err, rtapi.ErrCrossTenantSubscribe)
}

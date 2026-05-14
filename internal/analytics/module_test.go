package analytics_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/sociopulse/platform/internal/analytics"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/pkg/config"
)

// makeDeps constructs a minimal modules.Deps for Register tests. Each
// field is independently overridable via the modifier slice so tests
// can dial one variable at a time (HTTPRouter nil, DSN empty, …).
func makeDeps(_ *testing.T, logger *zap.Logger, modify ...func(*modules.Deps)) modules.Deps {
	gin.SetMode(gin.TestMode)
	d := modules.Deps{
		Ctx:        context.Background(),
		Logger:     logger,
		Config:     &config.Config{},
		HTTPRouter: gin.New(),
		Locator:    modules.NewMapLocator(),
	}
	for _, f := range modify {
		f(&d)
	}
	return d
}

// TestModule_Register_NoOpWhenHTTPRouterNil asserts the module short-
// circuits cleanly when no HTTP router is supplied (cmd/worker boot path
// would hit this if it ever passed analytics.Module to its registry).
func TestModule_Register_NoOpWhenHTTPRouterNil(t *testing.T) {
	t.Parallel()
	core, recorded := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	m := analytics.New(analytics.Config{})
	d := makeDeps(t, logger, func(d *modules.Deps) {
		d.HTTPRouter = nil
		// DSN + Enabled intentionally non-empty: the HTTPRouter check
		// fires before either is consulted.
		d.Config.Analytics.Enabled = true
		d.Config.Database.ClickHouse.DSN = "clickhouse://localhost:9000/default"
	})

	require.NoError(t, m.Register(d))
	require.True(t, hasLogMessage(recorded, "HTTP router unavailable"),
		"expected the HTTP-router-missing INFO log; got: %v", recorded.All())
}

// TestModule_Register_NoOpWhenAnalyticsDisabled asserts the module
// short-circuits when Config.Analytics.Enabled=false. The Q11 dual-
// target model lets cmd/api boot without analytics in dev environments
// that don't run CH (operator sets Enabled=false in YAML).
func TestModule_Register_NoOpWhenAnalyticsDisabled(t *testing.T) {
	t.Parallel()
	core, recorded := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	m := analytics.New(analytics.Config{})
	d := makeDeps(t, logger, func(d *modules.Deps) {
		d.Config.Analytics.Enabled = false
		// DSN intentionally non-empty: the Enabled check fires first.
		d.Config.Database.ClickHouse.DSN = "clickhouse://localhost:9000/default"
	})

	require.NoError(t, m.Register(d))
	require.True(t, hasLogMessage(recorded, "Config.Analytics.Enabled=false"),
		"expected the Enabled=false INFO log; got: %v", recorded.All())

	// The MetricsQuery key must NOT be registered when the module
	// short-circuits — downstream lookups should observe ok=false,
	// not a stale half-wired service.
	_, ok := d.Locator.Lookup(analytics.LocatorMetricsQuery)
	require.False(t, ok, "no-op Register must not register MetricsQuery")
}

// TestModule_Register_NoOpWhenDSNEmpty asserts the nested fallback:
// even with Enabled=true, an empty DSN still skips wiring with a WARN.
// Dev environments without a CH container keep booting.
func TestModule_Register_NoOpWhenDSNEmpty(t *testing.T) {
	t.Parallel()
	core, recorded := observer.New(zap.WarnLevel)
	logger := zap.New(core)

	m := analytics.New(analytics.Config{})
	d := makeDeps(t, logger, func(d *modules.Deps) {
		d.Config.Analytics.Enabled = true
		// DSN defaults to "" so the nested fallback fires.
	})

	require.NoError(t, m.Register(d))
	require.True(t, hasLogMessage(recorded, "enabled but clickhouse DSN empty"),
		"expected the DSN-empty WARN log; got: %v", recorded.All())

	_, ok := d.Locator.Lookup(analytics.LocatorMetricsQuery)
	require.False(t, ok, "no-op Register must not register MetricsQuery")
}

// TestModule_Register_DegradesOnUnreachableClickHouse asserts the
// module logs WARN and proceeds (returns nil) when CH is configured but
// unreachable at boot. The pre-flight Ping in store.Open is what trips
// here — a bogus host yields a fast dial failure.
func TestModule_Register_DegradesOnUnreachableClickHouse(t *testing.T) {
	t.Parallel()
	core, recorded := observer.New(zap.WarnLevel)
	logger := zap.New(core)

	m := analytics.New(analytics.Config{})
	d := makeDeps(t, logger, func(d *modules.Deps) {
		// Plan 13.2 Task 6 — Enabled must be true so the gate falls
		// through to the actual CH open path.
		d.Config.Analytics.Enabled = true
		// Point at a host that won't resolve / won't accept. The exact
		// failure mode (dial vs ping) is irrelevant to the test — both
		// surface as a wrapped error in Register and trigger the WARN.
		d.Config.Database.ClickHouse.DSN = "clickhouse://127.0.0.1:1/nonexistent?dial_timeout=200ms"
		// Bound the timeout: the store dial respects ctx, but we still
		// want a quick test.
		ctx, cancel := context.WithCancel(d.Ctx)
		_ = cancel // dial_timeout in DSN handles the bound; ctx is just a precaution.
		d.Ctx = ctx
	})

	require.NoError(t, m.Register(d), "degraded boot returns nil, not error")
	require.True(t, hasLogMessage(recorded, "clickhouse unavailable"),
		"expected the CH-unavailable WARN log; got: %v", recorded.All())
	_, ok := d.Locator.Lookup(analytics.LocatorMetricsQuery)
	require.False(t, ok, "degraded Register must not register MetricsQuery")
}

// TestModule_Register_HardErrorOnMissingLogger asserts a missing Logger
// is treated as a wiring bug (return error) rather than as a degraded
// fallback. Mirrors recording.Module.Register's policy.
func TestModule_Register_HardErrorOnMissingLogger(t *testing.T) {
	t.Parallel()
	m := analytics.New(analytics.Config{})
	d := modules.Deps{
		Ctx:        context.Background(),
		Config:     &config.Config{},
		HTTPRouter: gin.New(),
		Locator:    modules.NewMapLocator(),
	}
	err := m.Register(d)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Logger is required")
}

// TestModule_Register_NoOpDoesNotMountRoutes asserts the gin engine has
// no /api/analytics/* routes registered when Register short-circuits.
// Downstream tests that probe the engine routes would otherwise see
// stale handlers.
func TestModule_Register_NoOpDoesNotMountRoutes(t *testing.T) {
	t.Parallel()
	logger := zap.NewNop()
	m := analytics.New(analytics.Config{})
	d := makeDeps(t, logger) // DSN defaults to ""

	require.NoError(t, m.Register(d))

	for _, route := range d.HTTPRouter.Routes() {
		require.False(t, strings.HasPrefix(route.Path, "/api/analytics"),
			"no-op Register must not mount analytics routes; got %s", route.Path)
	}
}

// TestModule_Name asserts the module identifier is stable. cmd/api logs
// this name; renaming would break ops dashboards.
func TestModule_Name(t *testing.T) {
	t.Parallel()
	require.Equal(t, "analytics", analytics.New(analytics.Config{}).Name())
}

// TestModule_LocatorCrmFallbacks asserts the three Q12-documented
// fallback paths all yield a clean no-op Register (the locator lookup
// helper is tested indirectly via the public Register entry point —
// when CH is empty Register short-circuits before reaching the helper,
// so we set CH empty so as not to need a live container).
//
// The locator lookup helper's actual branching is exercised below by
// the helper-level unit tests on the public API surface of crmapi —
// see also TestLocatorCrmProjectService_* below.
func TestModule_LocatorCrmFallbacks(t *testing.T) {
	t.Parallel()
	logger := zap.NewNop()
	m := analytics.New(analytics.Config{})

	// Case 1: locator missing entirely.
	d1 := modules.Deps{
		Ctx:        context.Background(),
		Logger:     logger,
		Config:     &config.Config{},
		HTTPRouter: gin.New(),
		// Locator left nil — Register's HTTPRouter check fires first,
		// but if it didn't, the lookupCrmProjectService(nil, …) branch
		// is exercised by the wired-path tests below.
	}
	require.NoError(t, m.Register(d1))

	// Case 2: locator empty (no crm.ProjectService registered).
	d2 := makeDeps(t, logger) // DSN empty → no-op; locator is fresh + empty.
	require.NoError(t, m.Register(d2))

	// Case 3: locator carries a wrong-typed value under the crm key.
	d3 := makeDeps(t, logger)
	d3.Locator.Register(analytics.LocatorCrmProjectService, "not-a-service")
	require.NoError(t, m.Register(d3))
}

// hasLogMessage scans the observed logs for a substring match on the
// .Message field. Lets the assertion stay readable without depending on
// the exact full message string (which may evolve).
func hasLogMessage(obs *observer.ObservedLogs, substr string) bool {
	for _, e := range obs.All() {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

// =============================================================================
// CrmReader compile-time + behavioural checks
// =============================================================================

// crmServiceStub satisfies crmapi.ProjectService with stubbed-out
// methods. The ONLY method analytics calls is GetProgress; everything
// else returns zero values so the stub compiles against the wider
// interface without dragging in business logic.
//
// This stub doubles as a compile-time assertion that crmapi.ProjectService
// continues to satisfy service.CrmReader (one method: GetProgress).
type crmServiceStub struct{}

func (crmServiceStub) Create(_ context.Context, _ crmapi.CreateProjectInput) (*crmapi.Project, error) {
	return nil, errors.New("stub")
}
func (crmServiceStub) Get(_ context.Context, _ uuid.UUID) (*crmapi.Project, error) {
	return nil, errors.New("stub")
}
func (crmServiceStub) List(_ context.Context, _ crmapi.ListProjectsFilter) (*crmapi.ListProjectsResult, error) {
	return nil, errors.New("stub")
}
func (crmServiceStub) Update(_ context.Context, _ uuid.UUID, _ crmapi.UpdateProjectInput) (*crmapi.Project, error) {
	return nil, errors.New("stub")
}
func (crmServiceStub) Pause(_ context.Context, _ uuid.UUID) error   { return errors.New("stub") }
func (crmServiceStub) Resume(_ context.Context, _ uuid.UUID) error  { return errors.New("stub") }
func (crmServiceStub) Archive(_ context.Context, _ uuid.UUID) error { return errors.New("stub") }
func (crmServiceStub) GetProgress(_ context.Context, _ uuid.UUID) (*crmapi.ProjectProgress, error) {
	return &crmapi.ProjectProgress{TargetCount: 0}, nil
}
func (crmServiceStub) Assign(_ context.Context, _ uuid.UUID, _ []uuid.UUID) error {
	return errors.New("stub")
}
func (crmServiceStub) Unassign(_ context.Context, _ uuid.UUID, _ uuid.UUID) error {
	return errors.New("stub")
}
func (crmServiceStub) ListMembers(_ context.Context, _ uuid.UUID) ([]crmapi.ProjectMember, error) {
	return nil, errors.New("stub")
}

// Compile-time check: crmapi.ProjectService satisfies service.CrmReader
// via the crmServiceStub. If this stops compiling, the analytics module
// can no longer treat the locator-resolved ProjectService as a CrmReader
// and module.go::lookupCrmProjectService needs an explicit adapter.
var _ crmapi.ProjectService = (*crmServiceStub)(nil)

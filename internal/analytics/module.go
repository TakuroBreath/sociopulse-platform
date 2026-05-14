// Package analytics — Module registration entry point.
//
// Plan 13.2 Task 5 fills this in: Register builds the read-side query
// path (CH conn + Redis cache + crm port lookup → QueryService),
// registers it under LocatorMetricsQuery for Plan 13.3 reports, and
// mounts the six /api/analytics/* GET routes on Deps.HTTPRouter.
//
// Composition model (per Plan 13.2 § Q11 dual-target):
//
//   - cmd/api walks the modules.Registry and invokes Register on every
//     module including this one. cmd/api ONLY needs the query path —
//     HTTP routes + MetricsQuery — so Register's job is bounded to that.
//   - cmd/worker constructs the IngestPipeline directly via the
//     analytics/wire package (Plan 13.2 Task 6, separate). The ingest
//     side does NOT go through this Module.Register.
//
// Degraded-boot story:
//
//   - HTTPRouter nil  → log INFO + skip; nothing else to wire.
//   - ClickHouse DSN empty → log INFO + skip; cmd/api still serves
//     /healthz / /metrics / other modules' routes.
//   - ClickHouse Open fails → log WARN + skip; same fallback.
//   - Redis nil → cache becomes a no-op (RedisCache short-circuits on
//     nil rdb); every query hits CH directly. Acceptable for boot.
//   - crm.ProjectService not in locator → CrmReader=nil; RegionProgress
//     falls back to Plan=0 (Q12 documented behaviour).
//
// Gating (Plan 13.2 Task 6 finalised):
//
//   - Config.Analytics.Enabled is the canonical "should this module
//     wire?" gate. Disabled → log INFO + skip the entire module.
//   - DSN-empty is a NESTED fallback even when Enabled=true. Dev
//     environments without a CH container still boot cmd/api cleanly
//     (log WARN, skip the rest of Register).
//   - QueryConfig values (CacheShortTTL, CacheLongTTL,
//     LongWindowThreshold) are now pulled from d.Config.Analytics
//     instead of hardcoded constants.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/analytics/metrics"
	"github.com/sociopulse/platform/internal/analytics/service"
	"github.com/sociopulse/platform/internal/analytics/store"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/modules"
)

// LocatorMetricsQuery is the canonical locator key under which Register
// registers the constructed *QueryService. Plan 13.3 reports module
// looks up this key when it needs to drill from a report job into a
// dashboard query.
const LocatorMetricsQuery = "analytics.MetricsQuery"

// LocatorCrmProjectService is the locator key for the crm.ProjectService
// reference Register resolves at boot. Mirrors recording.LocatorRecordingService
// / auth.LocatorClaimsValidator — every cross-module port reads its key
// from a constant on the producing module. This constant duplicates
// crm.LocatorProjectService deliberately: analytics reads from the
// locator at Register time, so the source of the key is in this file's
// dependency footprint — no import-cycle of internal/crm.
//
// If the crm module ever renames its key, the analytics module will
// see "not in locator" at boot and silently fall through to
// CrmReader=nil + Plan=0 (Q12 fallback). The locator is intentionally
// late-bound for this reason.
const LocatorCrmProjectService = "crm.ProjectService"

// Config is the construction-time configuration for the analytics
// module. Today only the prometheus registerer is required; future
// fields (cache TTL, threshold) may be added when Plan 13.2 Task 6
// lands a typed AnalyticsConfig in pkg/config.
type Config struct {
	// Registerer is the prometheus registerer the query metrics attach
	// to. May be nil — the module falls back to a private registry so
	// tests boot cleanly without collector duplicates.
	Registerer prometheus.Registerer
}

// Module is the top-level registration handle for the analytics module.
// New(cfg) returns a fresh instance; Register wires the read path; the
// module owns no goroutines and needs no Start/Stop lifecycle (ingest
// runs in cmd/worker, not here).
type Module struct {
	cfg Config
}

// Compile-time check that Module satisfies modules.Module — catches
// signature drift at build time.
var _ modules.Module = (*Module)(nil)

// New returns a fresh Module ready for Register. cmd/api passes
// pkg/observability.Metrics.Registry as cfg.Registerer so the analytics
// query collectors land on the shared /metrics endpoint.
//
// The zero-value Module{} is also usable — Register falls back to a
// private prometheus registry. Used by cmd/api during dev wiring and
// by the module_test.go suite.
func New(cfg Config) *Module {
	return &Module{cfg: cfg}
}

// Name returns the module's unique identifier within the registry.
func (*Module) Name() string { return "analytics" }

// Register wires the read-side query path: CH conn → Redis cache → crm
// port → QueryService → locator registration → HTTP route mount.
//
// Every dependency is degraded-boot tolerant — see the package doc
// comment for the fallback matrix.
func (m *Module) Register(d modules.Deps) error {
	if d.Logger == nil {
		return errors.New("analytics: Deps.Logger is required")
	}
	logger := d.Logger.Named("analytics")

	if d.HTTPRouter == nil {
		logger.Info("analytics: HTTP router unavailable, module disabled")
		return nil
	}

	if d.Config == nil {
		logger.Info("analytics: config nil, module disabled")
		return nil
	}

	// Plan 13.2 Task 6: Config.Analytics.Enabled is the canonical
	// gate. When false the module skips wiring entirely; cmd/api still
	// serves /healthz / /metrics / other modules' routes.
	if !d.Config.Analytics.Enabled {
		logger.Info("analytics: Config.Analytics.Enabled=false, module disabled")
		return nil
	}

	if d.Config.Database.ClickHouse.DSN == "" {
		// Nested fallback: even when Enabled=true, an empty DSN means
		// no ClickHouse to talk to. WARN (operator misconfiguration)
		// but don't fail boot — cmd/api keeps serving the rest.
		logger.Warn("analytics: enabled but clickhouse DSN empty, module disabled")
		return nil
	}

	chConn, err := openCH(d, logger)
	if err != nil {
		// Degraded boot: CH unreachable at startup → log + skip the
		// whole module. cmd/api still serves the rest of /api/*.
		logger.Warn("analytics: clickhouse unavailable — HTTP routes skipped",
			zap.Error(err))
		return nil
	}

	// Build the query metrics collectors. Failure to register is a hard
	// error — duplicate collector names indicate a wiring bug, not a
	// degraded environment.
	queryMetrics, err := metrics.RegisterQueryMetrics(m.cfg.registerer())
	if err != nil {
		_ = chConn.Close()
		return fmt.Errorf("analytics: register query metrics: %w", err)
	}

	cache := service.NewRedisCache(d.Redis, logger.Named("cache"))

	crmReader := lookupCrmProjectService(d.Locator, logger)

	qs, err := service.NewQueryService(
		&service.StoreReaderAdapter{Conn: chConn},
		cache,
		crmReader,
		logger.Named("query"),
		queryMetrics,
		service.QueryConfig{
			CacheShortTTL:       d.Config.Analytics.CacheShortTTL,
			CacheLongTTL:        d.Config.Analytics.CacheLongTTL,
			LongWindowThreshold: d.Config.Analytics.LongWindowThreshold,
		},
	)
	if err != nil {
		_ = chConn.Close()
		return fmt.Errorf("analytics: build QueryService: %w", err)
	}

	if d.Locator != nil {
		d.Locator.Register(LocatorMetricsQuery, qs)
	}

	service.MountAnalyticsRoutes(d.HTTPRouter, qs, logger.Named("http"), queryMetrics)
	logger.Info("analytics: HTTP routes mounted under /api/analytics/*")
	return nil
}

// registerer returns a non-nil prometheus.Registerer — falling back to
// a private prometheus.NewRegistry when the caller didn't supply one.
// Mirrors recording.Config.registerer().
func (c Config) registerer() prometheus.Registerer {
	if c.Registerer != nil {
		return c.Registerer
	}
	return prometheus.NewRegistry()
}

// openCH builds the analytics/store.Conn from Deps.Config. The query
// side of the analytics module does not use BatchSize / FlushInterval
// (those drive the ingest pipeline that lives in cmd/worker), but
// store.Config.Validate requires them to be > 0 — supply 1 / 1s as
// inert placeholders. The DialTimeout is the only field that affects
// the read path.
func openCH(d modules.Deps, logger *zap.Logger) (*store.Conn, error) {
	chCfg := store.Config{
		DSN:           d.Config.Database.ClickHouse.DSN,
		BatchSize:     1,           // unused for the query side; placeholder for Config.Validate
		FlushInterval: time.Second, // unused for the query side; placeholder for Config.Validate
		DialTimeout:   5 * time.Second,
		Logger:        logger.Named("store"),
	}
	openCtx, cancel := context.WithTimeout(d.Ctx, 5*time.Second)
	defer cancel()
	return store.Open(openCtx, chCfg)
}

// lookupCrmProjectService resolves crm.ProjectService from the locator
// and returns a service.CrmReader adapter. Three outcomes:
//
//	locator missing entirely        → nil + INFO log; Plan=0 fallback (Q12)
//	entry absent                    → nil + INFO log; Plan=0 fallback (Q12)
//	entry present but wrong type    → nil + WARN log; Plan=0 fallback (Q12)
//	entry present and right type    → adapter; Plan from crm.GetProgress
func lookupCrmProjectService(loc modules.ServiceLocator, logger *zap.Logger) service.CrmReader {
	if loc == nil {
		logger.Info("analytics: locator nil — crm.ProjectService unwired, RegionProgress.Plan=0 (Q12 fallback)")
		return nil
	}
	raw, ok := loc.Lookup(LocatorCrmProjectService)
	if !ok {
		logger.Info("analytics: crm.ProjectService not in locator — RegionProgress.Plan=0 (Q12 fallback)",
			zap.String("locator_key", LocatorCrmProjectService))
		return nil
	}
	svc, ok := raw.(crmapi.ProjectService)
	if !ok {
		logger.Warn("analytics: crm.ProjectService locator entry has wrong type — RegionProgress.Plan=0 (Q12 fallback)",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil
	}
	// crmapi.ProjectService satisfies service.CrmReader (one method:
	// GetProgress). The compile-time assertion lives in module_test.go
	// to avoid a hard dependency on the wider ProjectService surface
	// in the production code path.
	return svc
}

// Package billing — Module registration entry point.
//
// Composition model (mirrors internal/reports/module.go and
// internal/analytics/module.go):
//
//   - cmd/api walks the modules.Registry, calls Module.Register with
//     Deps. Register validates the runtime tariff defaults, builds the
//     per-tenant pgx store, composes the service aggregate (Spend,
//     Margin, Revenue, Tariffs, CallFinalizedHook), mounts HTTP routes
//     under /api/finance and /api/billing, registers the
//     dialer.call.finalized NATS subscriber, and publishes the service
//     surface onto the locator under "billing.*" keys for downstream
//     consumers (cmd/worker has none today — Plan 14.x adds a billing
//     recompute worker stub).
//
//   - Module load order: billing depends on auth (RBACChecker) for the
//     PATCH /api/billing/tariffs admin check. cmd/api MUST register the
//     auth module BEFORE billing so the locator carries
//     auth.RBACChecker when Register runs. When auth is absent (today's
//     cmd/api bootstrap), the fast-path role match still permits
//     admin+supervisor for view actions and admin for tariff PATCH;
//     authenticated non-admin callers fall through to a fail-closed 403.
//
// Degraded-boot story (mirrors reports/analytics):
//
//	d.Logger nil           → hard error (composition-root invariant)
//	d.Config nil           → hard error
//	d.Pool nil             → INFO log + skip (worker-only boot, no DB)
//	d.HTTPRouter nil       → INFO log + skip (no HTTP surface to mount)
//	auth.RBACChecker miss  → WARN; HTTP still mounts with role-fast-path
//	d.Subscriber nil       → WARN; HTTP still mounts; NATS skipped
//	d.Locator nil          → WARN; HTTP + subscriber still wire; skips
//	                         locator publishing
//	cfg.Defaults invalid   → hard error (defence in depth — config.Validate
//	                         also runs at load time)
//
// The module owns no long-running goroutines in cmd/api — the NATS
// subscriber is dispatcher-driven by the bus. No Start/Stop lifecycle.
package billing

import (
	"errors"
	"fmt"

	"go.uber.org/zap"

	authmod "github.com/sociopulse/platform/internal/auth"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	billingsvc "github.com/sociopulse/platform/internal/billing/service"
	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
	httptransport "github.com/sociopulse/platform/internal/billing/transport/http"
	natstransport "github.com/sociopulse/platform/internal/billing/transport/nats"
	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/pkg/outbox"
)

// Locator keys this module publishes for downstream consumers.
const (
	// LocatorService publishes the *service.Service aggregate so future
	// modules (or a billing recompute worker) can call the read surface
	// without re-constructing it.
	LocatorService = "billing.Service"
	// LocatorTariffStore publishes the billingapi.TariffStore for
	// out-of-band tooling that needs the current tenant tariffs.
	LocatorTariffStore = "billing.TariffStore"
	// LocatorSpendReport publishes the billingapi.SpendReport.
	LocatorSpendReport = "billing.SpendReport"
	// LocatorMarginReport publishes the billingapi.MarginReport.
	LocatorMarginReport = "billing.MarginReport"
	// LocatorRevenueCalculator publishes the billingapi.RevenueCalculator.
	LocatorRevenueCalculator = "billing.RevenueCalculator"
	// LocatorCostCalculator publishes the billingapi.CostCalculator. The
	// pure-function surface is harmless to share — a dialer-side bonus
	// preview ("show the admin the projected cost") could use it directly.
	LocatorCostCalculator = "billing.CostCalculator"
	// LocatorCallFinalizedHook publishes the billingapi.CallFinalizedHook
	// so a future replay tool can drive the hook without re-wiring the
	// NATS subscriber.
	LocatorCallFinalizedHook = "billing.CallFinalizedHook"
)

// Module is the top-level registration handle for the billing module.
// Stateless; zero-value-safe — cmd/api passes billing.Module{} into the
// registry.
type Module struct{}

// Compile-time check that Module satisfies modules.Module.
var _ modules.Module = Module{}

// Name returns the module's unique identifier within the registry.
func (Module) Name() string { return "billing" }

// Register wires the billing module. See the package doc comment for the
// degraded-boot matrix.
//
//nolint:gocognit,gocyclo // composition-root style: linear sequence of locator pulls + degraded-boot fallbacks
func (Module) Register(d modules.Deps) error {
	if d.Logger == nil {
		return errors.New("billing: Deps.Logger is required")
	}
	logger := d.Logger.Named("billing")

	if d.Config == nil {
		return errors.New("billing: Deps.Config is required")
	}

	if d.Pool == nil {
		// No DB → no billing. Worker-only boot path falls through here;
		// cmd/api always supplies Pool.
		logger.Info("billing: Pool unavailable, module disabled")
		return nil
	}
	if d.HTTPRouter == nil {
		logger.Info("billing: HTTP router unavailable, module disabled")
		return nil
	}

	// Defence in depth — Config.Validate runs at load too, but a caller
	// constructing *Config directly (cmd/api tests, future embedders) may
	// bypass it. Surface a malformed defaults block at module-register
	// time rather than at the first finance request.
	cfg := d.Config.Billing
	if err := cfg.Defaults.Validate(); err != nil {
		return fmt.Errorf("billing: invalid Billing.Defaults: %w", err)
	}

	// RBAC checker — optional. Missing entry leaves requireRBAC's
	// role-fast-path permitting admin+supervisor for view actions and
	// admin for tariff PATCH; authenticated non-admin callers fall
	// through to a fail-closed 403. See internal/billing/transport/http
	// routes.go::requireRBAC for the exact semantics.
	rbac, rbacWired := lookupRBACChecker(d.Locator, logger)

	// Store layer. *PG satisfies SettingsBackend, AggregatorBackend,
	// MarginBackend, RevenueBackend, and CostSink simultaneously — every
	// service interface is implemented by the same pgx-backed struct.
	store := billingpgx.New(d.Pool)

	// Service layer — compose each component, then bundle into the
	// Service aggregate for HTTP handlers + locator publishing.
	calc := billingsvc.NewCostCalculator()
	tariffs := billingsvc.NewTariffStore(store, cfg.Defaults)
	revenue := billingsvc.NewRevenueCalculator(store)
	spend := billingsvc.NewSpendReport(store, tariffs, cfg.Defaults)
	margin := billingsvc.NewMarginReport(store, revenue)
	hook := billingsvc.NewCallFinalizedHandler(
		calc, tariffs, store, cfg.Defaults,
		logger.Named("oncallfinalized"),
	)

	svc := &billingsvc.Service{
		SpendReport:    spend,
		MarginReport:   margin,
		Revenue:        revenue,
		Tariffs:        tariffs,
		DefaultTariffs: cfg.Defaults,
		Logger:         logger,
	}

	// Audit emitter — best-effort outbox writer for the PATCH-tariffs
	// admin action. The writer is stateless (zero-value PostgresWriter);
	// no locator round-trip needed.
	auditWriter := outbox.NewPostgresWriter()
	audit := billingsvc.NewAuditEmitter(d.Pool, auditWriter, logger.Named("audit"))

	// HTTP transport. Routes mount under /api/finance (4 GETs) and
	// /api/billing/tariffs (GET + PATCH). NewHandlers's third arg is the
	// clock; nil → time.Now (production).
	handlers := httptransport.NewHandlers(svc, audit, nil)
	httptransport.Register(d.HTTPRouter, httptransport.RouterDeps{
		Handlers: handlers,
		RBAC:     rbac,
	})

	// NATS subscriber for tenant.*.dialer.call.finalized. The hook's
	// INSERT is idempotent (ON CONFLICT (call_id) DO NOTHING) so a
	// queue-group redelivery is harmless.
	subscriberWired := false
	if d.Subscriber == nil {
		logger.Warn("billing: Deps.Subscriber unavailable, cost ingestion disabled")
	} else {
		subscriber := natstransport.NewCallFinalizedSubscriber(hook, logger.Named("nats"))
		if err := subscriber.Subscribe(d.Ctx, d.Subscriber); err != nil {
			// Subscribe failure at boot is non-fatal for the module —
			// HTTP surface still works but cost ingestion is dead until
			// the bus recovers and cmd/api restarts. We log loudly so an
			// operator can spot the gap in metrics.
			logger.Warn("billing: NATS subscribe failed — cost ingestion disabled",
				zap.Error(err))
		} else {
			subscriberWired = true
		}
	}

	// Locator entries — for downstream modules / cmd/worker / a future
	// recompute tool to discover the billing surface without re-wiring.
	if d.Locator == nil {
		logger.Warn("billing: locator unavailable, service publishing skipped")
	} else {
		d.Locator.Register(LocatorService, svc)
		d.Locator.Register(LocatorTariffStore, tariffs)
		d.Locator.Register(LocatorSpendReport, spend)
		d.Locator.Register(LocatorMarginReport, margin)
		d.Locator.Register(LocatorRevenueCalculator, revenue)
		d.Locator.Register(LocatorCostCalculator, calc)
		d.Locator.Register(LocatorCallFinalizedHook, hook)
	}

	logger.Info("billing module registered",
		zap.Bool("rbac_wired", rbacWired),
		zap.Bool("subscriber_wired", subscriberWired),
		zap.Bool("locator_wired", d.Locator != nil),
	)
	return nil
}

// lookupRBACChecker pulls auth.RBACChecker out of the locator. Mirrors
// internal/reports/module.go::lookupRBACChecker — returns (nil, false)
// when missing or type-mismatched so requireRBAC degrades to a
// role-fast-path-only check (still safe: admin/supervisor short-circuit,
// non-elevated callers fall through to 403).
func lookupRBACChecker(loc modules.ServiceLocator, logger *zap.Logger) (authapi.RBACChecker, bool) {
	if loc == nil {
		logger.Warn("billing: locator unavailable, auth.RBACChecker lookup skipped")
		return nil, false
	}
	raw, ok := loc.Lookup(authmod.LocatorRBACChecker)
	if !ok {
		logger.Warn("billing: auth.RBACChecker not registered, RBAC degrades to role-fast-path only")
		return nil, false
	}
	checker, ok := raw.(authapi.RBACChecker)
	if !ok {
		logger.Error("billing: auth.RBACChecker registered with wrong type — RBAC degrades",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil, false
	}
	return checker, true
}

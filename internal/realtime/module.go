// Package realtime is the registration entry point for the realtime
// module — WebSocket Hub + NATS dispatcher + Redis presence + listen-in.
//
// Plan 11 wires this in 10 sub-tasks:
//  1. Module skeleton + interfaces in internal/realtime/api (Plan 11
//     Task 1 — added the AllTopics / TopicAction registry helpers + the
//     Module.New constructor).
//  2. WebSocket connection lifecycle (auth handshake, writer/reader
//     goroutines, slow-consumer drop-oldest).
//  3. Hub fan-out + per-topic RBAC.
//  4. NATS subscriber + dispatcher (a) JetStream Publisher/Subscriber
//     in pkg/eventbus, (b) realtime/events dispatcher, (c) THIS task —
//     wire the dispatcher + Hub into cmd/api with errgroup-driven
//     lifecycle.
//  5. Redis-backed PresenceTracker.
//  6. Listen-in v1 (silent mode) + audit (DEFERRED until Plan 08
//     FreeSWITCH cluster lands; stub returns ErrTelephonyBridgeOffline).
//  7. HTTP handlers (/ws, listen-in endpoints, force-action endpoints).
//  8. Helm + ingress timeouts (DEFERRED to sociopulse-infra repo).
//  9. Integration tests + coverage.
//
// 10. Frame classification + listen-in cleanup on disconnect + janitor.
//
// Plus carry-overs from prior plans (handled here):
//   - internal/telephony/nats_bridge — Plan 09 stub becomes real here.
//   - cmd/api outbox publisher — noop swapped for real NATS.
//   - dialer transport/http — RefreshPresence wired into middleware.
//   - dialer SnapshotPubSub — in-mem swapped for NATS-backed fan-out.
//
// See docs/references/plan-11-realtime.md for the canonical reading
// list, gotchas, and architecture decisions.
//
// # Plan 11 Task 4c — composition
//
// Register builds the Hub + per-connection metrics + topic RBAC and
// stashes the Hub under rtapi.LocatorHub so Plan 11 Task 7 (the WS
// HTTP handler) can resolve it without taking a transitive dependency
// on internal/realtime/service. The dispatcher itself is NOT
// constructed here — per the plan-11-realtime.md gotcha at line 97 it
// lives in cmd/api so its Start/Stop is errgroup-driven from the
// composition root.
package realtime

import (
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/internal/modules"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
	transporthttp "github.com/sociopulse/platform/internal/realtime/transport/http"
)

// locatorAuthAuthenticator is the locator key the auth module
// registers its Authenticator implementation under. Mirrored here
// (rather than imported from internal/auth/module.go) to keep the
// realtime module's compile-time deps minimal — module.go for auth
// pulls in the entire auth/service stack.
const locatorAuthAuthenticator = "auth.Authenticator"

// locatorAuthClaimsValidator is the locator key for the auth module's
// ClaimsValidator. Same rationale as locatorAuthAuthenticator.
const locatorAuthClaimsValidator = "auth.ClaimsValidator"

// Config groups the construction-time parameters that don't fit on
// modules.Deps. Today only the Prometheus registerer goes through
// here; future Plan 11 tasks may extend this with WS handshake config,
// presence sweeper interval, etc.
//
// Registerer may be nil — Register falls back to a private
// prometheus.NewRegistry so the boot path doesn't panic in tests that
// don't bother to wire one. cmd/api in production passes
// pkg/observability.Metrics.Registry so realtime collectors land on
// the shared /metrics endpoint.
type Config struct {
	// Registerer is where Hub + Connection metrics are registered.
	// Nil-safe — Register builds a throw-away registry in that case.
	Registerer prometheus.Registerer
}

// Module is the top-level registration handle for the realtime module.
// Holds the lifecycle-owned components built in Register; Stop tears
// them down. Safe to construct as a zero value via New.
type Module struct {
	cfg Config

	// mu guards lifecycle bookkeeping — registered, stopped, and the
	// Hub reference Stop reads back to call Shutdown.
	mu         sync.Mutex
	logger     *zap.Logger
	hub        *service.Hub
	presence   *service.RedisPresenceTracker
	replicaID  string
	registered bool
	stopped    bool
}

// Compile-time assertion that *Module satisfies the modules.Module
// contract. Mirrors the pattern used by tenancy / dialer.
var _ modules.Module = (*Module)(nil)

// New returns a fresh Module ready for Register. cmd/api passes its
// pkg/observability.Metrics.Registry as cfg.Registerer so the realtime
// collectors land on the shared /metrics endpoint; tests typically
// pass prometheus.NewRegistry() to keep registrations isolated.
func New(cfg Config) *Module {
	return &Module{cfg: cfg}
}

// Name returns the module's unique identifier within the registry.
// Used by modules.Registry for ordering, locator key prefixes, and the
// /admin/modules HTTP endpoint.
func (*Module) Name() string { return "realtime" }

// Register wires the module's components into the composition root.
// It builds the Hub + per-topic RBAC + per-connection metrics and
// stashes the Hub under rtapi.LocatorHub. The dispatcher itself is
// NOT constructed here — per the plan-11-realtime.md gotcha at line 97
// the dispatcher's lifecycle lives in cmd/api so Start/Stop is
// errgroup-driven from the composition root.
//
// Required Deps:
//
//	d.Logger       — non-nil
//	d.Subscriber   — non-nil (fed into the dispatcher built in cmd/api;
//	                 Register itself does not call Subscribe)
//	d.Locator      — non-nil
//
// Optional Deps:
//
//	d.HTTPRouter   — Plan 11 Task 7 will mount /ws here.
//
// Register is idempotent: a second invocation returns nil without
// re-registering metrics or rebuilding the Hub. This guards against
// future refactors that re-run module init.
func (m *Module) Register(d modules.Deps) error {
	if err := requireDeps(d); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.registered {
		// Second call is a no-op — guards against accidental double
		// registration. The Hub already exists in the locator.
		return nil
	}

	logger := d.Logger.Named("realtime")
	m.logger = logger

	reg := m.cfg.Registerer
	if reg == nil {
		// No-op fallback — gives the test seam a private registry and
		// avoids the RegisterHubMetrics panic on a nil registerer.
		// cmd/api in production always supplies the shared registry.
		reg = prometheus.NewRegistry()
	}

	// Build the per-topic RBAC matrix once. The matrix is immutable
	// after construction; the Hub holds a pointer.
	//
	// Plan 11.2 Task 4-5: cross-tenant filter check via cached
	// resolvers. resolveResolversFromLocator returns empty fallbacks
	// when the production adapters aren't wired (degraded boot); the
	// empty fallback rejects every cross-tenant lookup so the security
	// envelope is bounded regardless. cmd/api's
	// registerRealtimeResolvers populates the locator entries before
	// this Register runs.
	rawUsers, rawProjects := resolveResolversFromLocator(d.Locator, logger)
	cachedUsers := service.NewCachedUserResolver(rawUsers, 0)
	cachedProjects := service.NewCachedProjectResolver(rawProjects, 0)
	rbac := service.NewTopicRBACWithResolvers(cachedUsers, cachedProjects)

	// Hub-level metrics + per-connection metrics. Both are registered
	// on the shared registry so dashboards can correlate
	// realtime_hub_* and realtime_dropped_frames_total without
	// joining across registries.
	hubMetrics := service.RegisterHubMetrics(reg)
	connMetrics := service.RegisterMetrics(reg)

	hub := service.NewHub(logger, hubMetrics, rbac)
	m.hub = hub

	// Stash the Hub + per-connection metrics in the locator so Plan 11
	// Task 7 (WS HTTP handler) can resolve them without importing
	// internal/realtime/service.
	d.Locator.Register(rtapi.LocatorHub, rtapi.Hub(hub))
	d.Locator.Register(rtapi.LocatorConnectionMetrics, connMetrics)

	// Plan 11 Task 5: Redis-backed PresenceTracker. Built only when
	// Deps.Redis is wired — Redis-less test setups (and any future
	// degraded-mode boot) skip presence cleanly. The HTTP handlers
	// look up rtapi.LocatorPresenceTracker and short-circuit when
	// missing.
	if d.Redis != nil {
		presenceMetrics := service.RegisterPresenceMetrics(reg)
		presence := service.NewRedisPresenceTracker(d.Redis, logger,
			service.WithPresenceMetrics(presenceMetrics))
		m.presence = presence
		d.Locator.Register(rtapi.LocatorPresenceTracker, rtapi.PresenceTracker(presence))
		logger.Info("realtime: presence tracker registered")
	} else {
		// Best-effort fallback: no Redis means no cross-replica
		// presence map. The handler that consumes the locator entry
		// must guard against the missing key. We log INFO (not WARN)
		// because Redis-less boot is a legitimate test/dev mode.
		logger.Info("realtime: presence tracker skipped (Redis unavailable)")
	}

	// Plan 11 Task 7: HTTP transport (WebSocket + force-action +
	// listen-in stubs). Mounted only when both the HTTP router AND
	// the auth module's Authenticator + ClaimsValidator have already
	// been registered in the locator. Auth registers itself BEFORE
	// realtime in cmd/api's Registry order, so the lookup should
	// always succeed in production; in degraded test boots we log a
	// warn and continue without the routes.
	m.replicaID = uuid.NewString()
	if err := m.mountHTTP(d, logger); err != nil {
		// mountHTTP returns nil on missing-but-tolerated
		// preconditions and an error only on a wiring contradiction
		// (e.g. auth lookup returned the wrong type). Surface to
		// cmd/api so the boot fails loudly.
		return err
	}

	m.registered = true

	logger.Info("realtime module registered (Plan 11 Task 4c+5)",
		zap.Bool("subscriber_wired", d.Subscriber != nil),
		zap.Bool("presence_wired", d.Redis != nil),
	)
	return nil
}

// Stop tears down the Hub, closing every registered connection with
// CloseGoingAway (1001). Safe to call multiple times — second
// invocation is a no-op. Safe to call before Register — the absence
// of a Hub short-circuits the Shutdown call.
//
// Stop does NOT unregister the Hub from the locator. cmd/api may
// inspect Hub.Stats() during the shutdown window to log final
// connection counts; the Hub remains usable for read-only operations
// after Shutdown.
func (m *Module) Stop() error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopped = true
	hub := m.hub
	logger := m.logger
	m.mu.Unlock()

	if hub != nil {
		hub.Shutdown()
	}
	if logger != nil {
		logger.Info("realtime module stopped")
	}
	return nil
}

// mountHTTP wires the HTTP transport on Deps.HTTPRouter when both the
// router AND the auth module's interfaces are present in the locator.
//
// The function is intentionally lenient on missing preconditions: a
// nil HTTPRouter, a missing Authenticator, or a missing
// ClaimsValidator produces a single WARN log and skips the mount. Any
// of those reflect a degraded test/boot mode, not a misconfiguration
// worth aborting registration over.
//
// The function ONLY returns an error when the locator entries exist
// but hold the wrong type — that's a contradiction (somebody
// registered "auth.Authenticator" with a non-Authenticator value) and
// surfacing it as a boot failure is the right behaviour.
func (m *Module) mountHTTP(d modules.Deps, logger *zap.Logger) error {
	if d.HTTPRouter == nil {
		logger.Info("realtime: HTTP transport skipped (HTTPRouter unavailable)")
		return nil
	}

	authRaw, ok := d.Locator.Lookup(locatorAuthAuthenticator)
	if !ok {
		logger.Warn("realtime: HTTP transport skipped — auth.Authenticator not in locator",
			zap.String("locator_key", locatorAuthAuthenticator),
		)
		return nil
	}
	authenticator, ok := authRaw.(authapi.Authenticator)
	if !ok {
		return fmt.Errorf("realtime: %s registered with wrong type %T",
			locatorAuthAuthenticator, authRaw)
	}

	cvRaw, ok := d.Locator.Lookup(locatorAuthClaimsValidator)
	if !ok {
		logger.Warn("realtime: HTTP transport skipped — auth.ClaimsValidator not in locator",
			zap.String("locator_key", locatorAuthClaimsValidator),
		)
		return nil
	}
	claimsValidator, ok := cvRaw.(authapi.ClaimsValidator)
	if !ok {
		return fmt.Errorf("realtime: %s registered with wrong type %T",
			locatorAuthClaimsValidator, cvRaw)
	}

	connMetricsRaw, _ := d.Locator.Lookup(rtapi.LocatorConnectionMetrics)
	connMetrics, _ := connMetricsRaw.(*service.Metrics)

	var presence rtapi.PresenceTracker
	if m.presence != nil {
		presence = m.presence
	}

	transporthttp.Mount(d.HTTPRouter.Group("/api/realtime"), transporthttp.Deps{
		Hub:             m.hub,
		AuthValidator:   transporthttp.NewAuthValidator(authenticator),
		ClaimsValidator: claimsValidator,
		ConnMetrics:     connMetrics,
		Presence:        presence,
		ReplicaID:       m.replicaID,
		Logger:          logger,
	})

	logger.Info("realtime: HTTP transport mounted",
		zap.String("replica_id", m.replicaID),
		zap.Bool("presence_wired", presence != nil),
	)
	return nil
}

// requireDeps validates that every Register prerequisite is non-nil.
// Returning a structured error (rather than panicking) lets cmd/api
// surface a clean message at boot.
func requireDeps(d modules.Deps) error {
	switch {
	case d.Logger == nil:
		return errors.New("realtime: Deps.Logger is required")
	case d.Locator == nil:
		return errors.New("realtime: Deps.Locator is required")
	case d.Subscriber == nil:
		// cmd/api must wire the JetStream subscriber (or a noop fallback
		// when NATS is down) before calling Module.Register; see
		// docs/references/plan-11-realtime.md gotcha line 97.
		return errors.New("realtime: Deps.Subscriber is required")
	}
	return nil
}

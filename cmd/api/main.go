// Package main is the entrypoint for cmd/api — the СоциоПульс monolith
// HTTP/WS/gRPC server.
//
// run() is the composition root: it loads configuration, builds telemetry
// (logger, tracer, metrics), opens infrastructure clients (DB, Redis, NATS),
// constructs the module Deps struct, registers every Module in dependency
// order, starts the HTTP and metrics listeners, runs platform-wide
// background goroutines (the outbox relay + realtime dispatcher), then
// waits on SIGINT/SIGTERM to drive graceful shutdown.
//
// Plan 11 Task 4c (this version) wires the real JetStream Publisher +
// Subscriber + the realtime Hub + dispatcher. The bring-up is best-effort:
// when NATS is unreachable the publisher falls back to noopPublisher, the
// dispatcher is skipped, and the gateway remains useful for /healthz /
// /readyz / /metrics. The realtime Hub is built unconditionally so future
// modules can resolve it via the locator even with no NATS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/sociopulse/platform/internal/analytics"
	"github.com/sociopulse/platform/internal/dialer"
	"github.com/sociopulse/platform/internal/healthz"
	healthchecks "github.com/sociopulse/platform/internal/healthz/checks"
	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/internal/realtime"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	rtevents "github.com/sociopulse/platform/internal/realtime/events"
	"github.com/sociopulse/platform/internal/recording"
	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/internal/recording/storage"
	recwire "github.com/sociopulse/platform/internal/recording/wire"
	"github.com/sociopulse/platform/internal/reports"
	"github.com/sociopulse/platform/internal/telephony"
	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/observability"
	"github.com/sociopulse/platform/pkg/outbox"
)

const (
	// defaultConfigDir is the fallback when neither --config-dir nor
	// SOCIOPULSE_CONFIG_DIR is set. In production this is overridden via the
	// k8s ConfigMap mount; in dev the working directory hosts ./configs.
	defaultConfigDir = "configs/development"

	// metricsShutdownGrace is the deadline given to the metrics listener
	// during shutdown. The metrics endpoint is local-only and lighter than
	// the public HTTP listener, so we cap it more aggressively.
	metricsShutdownGrace = 5 * time.Second
)

func main() {
	if err := run(rootContext(), parseConfigDir(os.Args[1:])); err != nil {
		// Logger may not be initialised yet on early failure, so we fall
		// back to stderr through fmt. Exit code 1 lets process supervisors
		// (k8s, systemd) detect a failure.
		_, _ = fmt.Fprintf(os.Stderr, "cmd/api: %v\n", err)
		os.Exit(1)
	}
}

// rootContext returns a context cancelled on SIGINT or SIGTERM. Splitting it
// out makes it easy to swap a plain context.Background() in tests via the
// run() seam.
func rootContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	return ctx
}

// parseConfigDir extracts --config-dir from argv with environment fallback.
// Kept tiny and side-effect free so tests that call run() directly never
// touch the global flag.CommandLine.
func parseConfigDir(args []string) string {
	fs := flag.NewFlagSet("cmd/api", flag.ExitOnError)
	dir := fs.String("config-dir", "", "path to the config directory containing config.yaml")
	_ = fs.Parse(args)
	if *dir != "" {
		return *dir
	}
	if env := os.Getenv("SOCIOPULSE_CONFIG_DIR"); env != "" {
		return env
	}
	return defaultConfigDir
}

// run is the composition root. It returns nil on graceful shutdown, error
// otherwise. ctx is cancelled by SIGINT/SIGTERM in production and by the
// test driver in cmd/api/main_test.go.
//
//nolint:gocognit // composition root is intentionally linear — splitting it further obscures the boot sequence
func run(ctx context.Context, configDir string) error {
	// 1. Load configuration. Hot-reload is enabled: subscribers (e.g. the
	//    auth module's rate-limit table) get a fresh Config on edit.
	snap, err := config.Load(config.LoadOptions{Dir: configDir, HotReload: true})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	defer func() { _ = snap.Close() }()
	cfg := snap.Get()

	// 2. Logger. Sync at the very end — stderr.Sync may fail in containers
	//    (ENOTTY), so we ignore the error per zap's own recommendation.
	logger, err := observability.NewLogger(cfg)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	// env/region/service are already attached as common fields by NewLogger;
	// only log the bits that vary across boots.
	logger.Info("cmd/api starting", zap.String("config_dir", configDir))

	// 3. Tracer. The OTLP exporter uses grpc.NewClient, which is non-blocking
	//    — a missing collector does not block startup; spans simply fail to
	//    export and surface in batch processor logs.
	tracer, tracerShutdown, err := observability.NewTracer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("init tracer: %w", err)
	}
	defer func() { //nolint:contextcheck // detached ctx is intentional for graceful drain after parent ctx cancel
		// Detached context — by the time this defer runs, the parent ctx
		// is already cancelled (SIGTERM); inheriting from it would make
		// the OTel exporter's drain return immediately without flushing.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.GracePeriod)
		defer cancel()
		if err := tracerShutdown(shutdownCtx); err != nil {
			logger.Warn("tracer shutdown", zap.Error(err))
		}
	}()

	// 4. Metrics registry. Cross-cutting collectors (HTTP, NATS, DB) live on
	//    *Metrics; module-specific collectors register through Metrics.Register
	//    at module-Register time.
	metrics := observability.NewMetrics(cfg)

	// 5. Infrastructure clients.
	//      - *postgres.Pool        → Plan 03 Task 4 (wired below)
	//      - *redis.Client         → Plan 04 (auth module)
	//      - *nats.Conn + JetStream → Plan 04 (eventbus); a noopPublisher
	//        stands in until then so the outbox relay can boot without
	//        NATS being available.
	//
	//    Pool open is non-blocking when MinConns is 0 (the default). A
	//    short Ping decides whether to start the outbox relay: in dev/test
	//    where Postgres is not running we skip the relay rather than
	//    flooding the log with connection-refused warnings. Production
	//    config sets MinConns >= 1 so failed dials surface here.
	var checks []healthz.Checker

	pool, pingErr := openPool(ctx, cfg, logger)
	if pool != nil {
		defer pool.Close()
		checks = append(checks, healthchecks.PostgresCheck{Pool: pool})
	}

	// 6. Redis. Best-effort — a missing Redis only disables the
	//    Redis-backed modules (auth refresh-token whitelist, dialer FSM
	//    + queue + heartbeat watchdog). Boot proceeds either way so the
	//    HTTP layer is still usable for /healthz / /readyz / /metrics.
	rdb, redisErr := openRedis(ctx, cfg, logger)
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
	}

	// 6b. NATS. Best-effort (mirrors Redis): a connection failure logs
	//     a WARN, the publisher falls back to noopPublisher, and the
	//     realtime dispatcher is skipped. Boot proceeds either way.
	//     URLs are redacted for log safety (mirrors redactDSN); only
	//     the host:port pair surfaces in info-level logs.
	natsPub, natsSub, natsErr := openNATS(ctx, cfg, logger)
	var (
		publisher  eventbus.Publisher
		subscriber eventbus.Subscriber
	)
	switch {
	case natsErr != nil:
		logger.Warn("nats unreachable; falling back to noop publisher/subscriber",
			zap.Strings("urls", redactNATSURLs(cfg.NATS.URLs)),
			zap.Error(natsErr),
		)
		publisher = newNoopPublisher(logger.Named("eventbus"))
		subscriber = newNoopSubscriber(logger.Named("eventbus"))
	default:
		logger.Info("nats publisher + subscriber up",
			zap.Strings("urls", redactNATSURLs(cfg.NATS.URLs)),
		)
		publisher = natsPub
		subscriber = natsSub
		// Healthz: only register the NATS check when we actually have
		// a real connection; the noop fallback would always report OK
		// and mask a misconfiguration.
		checks = append(checks, healthchecks.NATSCheck{Conn: natsPub})
	}
	// Lifecycle ordering — drain in REVERSE of construction. The Hub
	// shutdown defer below MUST run BEFORE these so it gets a clean
	// shot at closing every WS connection while the dispatcher is
	// still consuming. With Go's LIFO defer order, defining these
	// drains before the Hub-shutdown defer (which we add below in
	// step 9) gives us:
	//
	//   defer subscriber.Close()   // runs LAST (drained 4th)
	//   defer publisher.Close()    // runs LAST-1 (drained 3rd)
	//   defer dispatcher.Stop()    // runs LAST-2 (drained 2nd)
	//   defer realtimeModule.Stop()// runs FIRST after Hub close
	//   ...                        // ↑ Hub.Shutdown closes WS conns
	//
	// The errgroup's shutdown goroutine drains the HTTP server first;
	// this defer chain handles the NATS path explicitly.
	if natsPub != nil {
		defer func() {
			if err := natsPub.Close(); err != nil {
				logger.Warn("nats publisher close", zap.Error(err))
			}
		}()
	}
	if natsSub != nil {
		defer func() {
			if err := natsSub.Close(); err != nil {
				logger.Warn("nats subscriber close", zap.Error(err))
			}
		}()
	}

	// 7. HTTP server (gin engine + middleware + health endpoints). The engine
	//    is also stashed back on Deps so module Register() can mount routes.
	httpSrv, engine := buildHTTPServer(cfg, logger, tracer, metrics, checks) //nolint:contextcheck // server inherits ctx via gin handlers, not caller
	metricsSrv := buildMetricsServer(cfg, metrics)

	// 8. Module Deps. The locator is created up-front so order-
	//    dependent lookups (e.g. tenancy → auth → dialer) work the
	//    moment Module.Register starts being called.
	//
	//    EventBus is the real NATS publisher when available; otherwise
	//    the noop publisher. Subscriber follows the same fallback so
	//    realtime.Module.Register can reject a nil Subscriber as a
	//    composition-root wiring bug.
	locator := modules.NewMapLocator()
	deps := modules.Deps{
		Ctx:        ctx,
		Logger:     logger,
		Config:     &cfg,
		Pool:       pool,
		Redis:      rdb,
		EventBus:   publisher,
		Subscriber: subscriber,
		HTTPRouter: engine,
		Locator:    locator,
	}

	// 9. Module registry. Order matters — telephony registers the
	//    placeholder CommandPublisher under telephony.LocatorCommandPublisher
	//    before dialer.Register looks it up. realtime is registered
	//    AFTER dialer so any future module that wants to look up the
	//    realtime Hub via Deps.Locator can do so.
	//
	//    Plan 11.2 Task 5: providers (telephony/dialer + future
	//    auth/crm) register first, then registerRealtimeResolvers wires
	//    auth.UserService/crm.ProjectService onto the rtapi.UserResolver/
	//    ProjectResolver locator keys, then realtime.Module.Register
	//    looks them up via the locator and builds cached wrappers. The
	//    indirection preserves the scope rule that internal/realtime/*
	//    does not import internal/auth/* or internal/crm/* (Plan 11.1
	//    Task 2). Today's build has no auth/crm modules wired into
	//    cmd/api, so the resolvers fall back to the realtime module's
	//    empty-fallback path — which rejects every cross-tenant lookup
	//    and is strictly safer than no check.
	dialerModule := &dialer.Module{}
	realtimeModule := realtime.New(realtime.Config{Registerer: metrics.Registry})
	// Plan 12.1 Task 5 — recording module. Has no upstream module
	// deps (just Pool + Logger + Locator) so it joins the providers
	// walk. The gRPC façade is gated by cfg.Recording.Enabled — when
	// false (the dev default) the module registers RecordingService
	// in the locator but skips the listener.
	//
	// Plan 12.2 Task 5 — wire Local* ports for DEK unwrap + object
	// storage. wire.LocalPorts hex-decodes the KEK map from config and
	// fails boot fast on bad hex or wrong key length. An empty KEK
	// map yields a nil Ports + WARN — OpenAudioStream then returns
	// ErrInvalidInput "not wired" until Plan 01 wires the real Yandex
	// adapter.
	//
	// Plan 12.4 Task 5: helper relocated to internal/recording/wire so
	// cmd/worker can share the same construction path.
	recordingPorts, err := recwire.LocalPorts(cfg.Recording, logger.Named("recording"))
	if err != nil {
		return fmt.Errorf("recording ports: %w", err)
	}
	var (
		recordingDEK     crypto.DEKUnwrapper
		recordingObjects storage.ObjectStore
	)
	if recordingPorts != nil {
		recordingDEK = recordingPorts.DEK
		recordingObjects = recordingPorts.Objects
	}
	recordingModule := recording.New(recording.Config{
		Registerer:   metrics.Registry,
		GRPCConfig:   recordingGRPCConfig(cfg.Recording),
		DEKUnwrapper: recordingDEK,
		ObjectStore:  recordingObjects,
	})
	// Plan 13.2 Task 6 — analytics.Module{} joins the providers walk.
	// The module's Register is gated by cfg.Analytics.Enabled (Task 6);
	// when disabled it logs INFO and skips wiring. crm.Module is not in
	// the cmd/api boot list today, so the analytics module's
	// lookupCrmProjectService falls back to Plan=0 (Q12 documented
	// behaviour, exercised by TestModule_LocatorCrmFallbacks).
	// analytics.Module{} MUST come AFTER any future crm.Module so the
	// locator carries crm.ProjectService when Register runs.
	// Plan 13.3 Task 8 — reports.Module. MUST come AFTER analytics so the
	// locator carries analytics.MetricsQuery when Register runs. The
	// ObjectStore wired here is the same instance recording uses; nil
	// (e.g. dev env without local_keks) falls through to a WARN inside
	// reports.Register and the async Queue stays disabled.
	providers := modules.Registry{Modules: []modules.Module{
		telephony.Module{},
		dialerModule,
		recordingModule,
		analytics.New(analytics.Config{Registerer: metrics.Registry}),
		reports.New(reports.Config{ObjectStore: recordingObjects}),
	}}
	if err := registerModules(providers, deps, logger, redisErr); err != nil {
		return err
	}
	registerRealtimeResolvers(locator, logger)
	registerCallResolver(locator, logger) // Plan 11.4 Task 7
	consumers := modules.Registry{Modules: []modules.Module{
		realtimeModule,
	}}
	if err := registerModules(consumers, deps, logger, redisErr); err != nil {
		return err
	}
	defer func() {
		if err := dialerModule.Stop(); err != nil {
			logger.Warn("dialer module Stop failed", zap.Error(err))
		}
	}()
	defer func() {
		if err := recordingModule.Stop(); err != nil {
			logger.Warn("recording module Stop failed", zap.Error(err))
		}
	}()

	// 9b. Realtime dispatcher. Built ONLY when a real NATS subscriber
	//     is available — the noop subscriber would silently drop every
	//     message, so spawning the dispatcher against it wastes a
	//     goroutine and clutters the logs. Plan 11 gotcha at line 97:
	//     the dispatcher MUST live in cmd/api (NOT in
	//     realtime.Module.Register) so its Start/Stop is errgroup-driven.
	var dispatcher *rtevents.NATSSubscriber
	if natsSub != nil {
		hubVal, ok := locator.Lookup(rtapi.LocatorHub)
		if !ok {
			logger.Warn("realtime Hub missing from locator — dispatcher skipped")
		} else if hub, ok := hubVal.(rtevents.HubBroadcaster); !ok {
			logger.Warn("realtime Hub registered with wrong type — dispatcher skipped",
				zap.String("got_type", fmt.Sprintf("%T", hubVal)),
			)
		} else {
			eventsMetrics := rtevents.RegisterMetrics(metrics.Registry)
			// Plan 11.1 Task 2: cross-tenant fan-out for trunks.health.
			// The replicator looks up active tenants through the
			// tenancy.TenantService adapter; resolveTenantLister falls
			// back to an empty list when the tenancy module isn't
			// registered (minimal-boot path, integration tests).
			tenantLister := resolveTenantLister(locator, logger.Named("realtime.trunks"))
			trunksReplicator := rtevents.NewTrunksReplicator(
				hub,
				tenantLister,
				logger.Named("realtime.trunks"),
				eventsMetrics,
			)
			dispatcher = rtevents.NewNATSSubscriber(
				natsSub,
				hub,
				logger.Named("realtime.dispatcher"),
				eventsMetrics,
				rtevents.WithReplicaID(uuid.NewString()),
				rtevents.WithTrunksReplicator(trunksReplicator),
			)
		}
	} else {
		logger.Info("realtime dispatcher skipped: NATS subscriber unavailable")
	}
	// Defer chain — declarations in REVERSE shutdown order (LIFO).
	// Spec ordering: httpSrv.Shutdown → Hub.Shutdown → dispatcher.Stop
	// → subscriber.Close → publisher.Close.
	//
	// HTTP shutdown happens inside the errgroup goroutine on
	// gctx.Done; once g.Wait() returns the defers below fire LIFO:
	//
	//   defer realtimeModule.Stop  → fires FIRST  (Hub.Shutdown)
	//   defer dispatcher.Stop      → fires SECOND
	//   (existing) natsSub.Close   → fires THIRD  (drains subscriber)
	//   (existing) natsPub.Close   → fires FOURTH (drains publisher)
	if dispatcher != nil {
		defer func() {
			if err := dispatcher.Stop(); err != nil {
				logger.Warn("realtime dispatcher Stop failed", zap.Error(err))
			}
		}()
	}
	defer func() {
		if err := realtimeModule.Stop(); err != nil {
			logger.Warn("realtime module Stop failed", zap.Error(err))
		}
	}()

	// 10. Outbox relay.
	//
	//    Started only when Postgres is reachable: pingErr above tells us
	//    whether boot-time ping succeeded. Running this on every replica
	//    is intentional — the relay uses FOR UPDATE SKIP LOCKED so leader
	//    election is unnecessary; replicas naturally split the queue.
	var relay *outbox.Relay
	if pool != nil && pingErr == nil {
		relay = outbox.NewRelay(pool, publisher, outbox.RelayConfig{
			BatchSize:      cfg.Outbox.BatchSize,
			Tick:           cfg.Outbox.Tick,
			MaxRetry:       cfg.Outbox.MaxRetry,
			PublishTimeout: 5 * time.Second, //nolint:mnd // matches pkg/outbox default
		}, logger.Named("outbox"))
	} else {
		logger.Info("outbox relay skipped: Postgres unreachable",
			zap.Error(pingErr))
	}

	// 11. errgroup orchestration. Each long-running goroutine returns from
	//    its Run/ListenAndServe when the parent context is cancelled.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		logger.Info("http listener up", zap.String("bind", cfg.HTTP.Bind))
		if err := listenAndServe(httpSrv); err != nil {
			return fmt.Errorf("http listen: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		logger.Info("metrics listener up", zap.String("bind", cfg.Observability.Metrics.Bind))
		if err := listenAndServe(metricsSrv); err != nil {
			return fmt.Errorf("metrics listen: %w", err)
		}
		return nil
	})
	if relay != nil {
		g.Go(func() error {
			if err := relay.Run(gctx); err != nil {
				return fmt.Errorf("outbox relay: %w", err)
			}
			return nil
		})
	}
	if dispatcher != nil {
		// Dispatcher.Start is synchronous — it registers every subject
		// pattern with the bus and returns. The push-consumer
		// goroutines are owned by the underlying nats.go subscriber;
		// our errgroup goroutine just waits on gctx so the goroutine
		// exits with the rest at shutdown.
		g.Go(func() error {
			if err := dispatcher.Start(gctx); err != nil {
				return fmt.Errorf("realtime dispatcher start: %w", err)
			}
			<-gctx.Done()
			return nil
		})
	}
	// Plan 12.1 Task 5 — recording.Module.Start blocks on gctx whether
	// or not the gRPC listener was configured. When disabled it just
	// waits on shutdown (tracking the goroutine in the errgroup); when
	// enabled it runs the listener and surfaces Serve errors back to
	// the group so a TLS listener crash trips a clean shutdown.
	g.Go(func() error {
		if err := recordingModule.Start(gctx); err != nil {
			return fmt.Errorf("recording listener: %w", err)
		}
		return nil
	})

	// 12. Wait for shutdown signal — either ctx.Done (SIGTERM) or one of the
	//     errgroup goroutines failing.
	g.Go(func() error {
		<-gctx.Done()
		logger.Info("shutdown signal received, draining listeners")
		// Detached contexts — by the time we reach here the parent ctx
		// is cancelled, so we deliberately do NOT inherit it. Inheriting
		// would make Server.Shutdown abort all in-flight requests instead
		// of letting them drain.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.GracePeriod)
		defer cancel()
		//nolint:contextcheck // detached ctx is intentional for graceful drain
		shutdownServer(shutdownCtx, httpSrv, "http", logger)

		mctx, mcancel := context.WithTimeout(context.Background(), metricsShutdownGrace)
		defer mcancel()
		//nolint:contextcheck // detached ctx is intentional for graceful drain
		shutdownServer(mctx, metricsSrv, "metrics", logger)
		return nil
	})

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("server group: %w", err)
	}
	logger.Info("cmd/api shutdown complete")
	return nil
}

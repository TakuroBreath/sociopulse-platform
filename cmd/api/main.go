// Package main is the entrypoint for cmd/api — the СоциоПульс monolith
// HTTP/WS/gRPC server.
//
// run() is the composition root: it loads configuration, builds telemetry
// (logger, tracer, metrics), opens infrastructure clients (DB, Redis, NATS —
// stubbed until Plan 03/04), constructs the module Deps struct, registers
// every Module in dependency order, starts the HTTP and metrics listeners,
// runs platform-wide background goroutines (the outbox relay), then waits on
// SIGINT/SIGTERM to drive graceful shutdown.
//
// Subsequent plans plug into this skeleton:
//   - Plan 03 Task 6 (this commit): outbox relay wired with a noop
//     eventbus.Publisher; the relay drains event_outbox to that publisher
//     until Plan 04 swaps in the real NATS-backed Publisher.
//   - Plan 04+: each business module's Register() runs inside this run() —
//     they receive the same Deps and mount their handlers on the gin
//     engine via Deps.HTTPRouter.
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

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/sociopulse/platform/internal/healthz"
	healthchecks "github.com/sociopulse/platform/internal/healthz/checks"
	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/pkg/config"
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

	// 6. Module Deps. Modules will be added here in later plans. The locator
	//    is created up-front so order-dependent lookups (e.g. tenancy → auth
	//    → realtime) work the moment Module.Register starts being called.
	//
	//    EventBus is the noop publisher until Plan 04 wires NATS — it lets
	//    the outbox relay run end-to-end (drain rows, mark them published)
	//    without a NATS cluster being available. Modules that need NATS
	//    publishing today should wait for Plan 04.
	publisher := newNoopPublisher(logger.Named("eventbus"))
	locator := modules.NewMapLocator()
	deps := modules.Deps{
		Ctx:        ctx,
		Logger:     logger,
		Config:     &cfg,
		Pool:       pool,
		EventBus:   publisher,
		HTTPRouter: nil, // populated below once buildHTTPServer returns
		Locator:    locator,
	}
	registry := modules.Registry{Modules: nil} // populated by Plan 04+
	_ = deps                                   // silence unused-write warning until modules attach
	_ = registry

	// 7. HTTP server (gin engine + middleware + health endpoints). The engine
	//    is also stashed back on Deps so module Register() can mount routes.
	httpSrv := buildHTTPServer(cfg, logger, tracer, metrics, checks) //nolint:contextcheck // server inherits ctx via gin handlers, not caller
	// deps.HTTPRouter would be the *gin.Engine here once we expose it from
	// buildHTTPServer; deferred until a module needs it (Plan 04+).
	metricsSrv := buildMetricsServer(cfg, metrics)

	// 8. Outbox relay.
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

	// 9. errgroup orchestration. Each long-running goroutine returns from
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

	// 10. Wait for shutdown signal — either ctx.Done (SIGTERM) or one of the
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

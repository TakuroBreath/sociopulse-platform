// Package main is the entrypoint for cmd/telephony-bridge — the СоциоПульс
// ESL ↔ NATS bridge sidecar. It is a separate binary from cmd/api: the
// bridge owns the only ESL connections to the FreeSWITCH fleet, dispatches
// commands published on tenant.<t>.telephony.cmd.<call_id>, and re-publishes
// FS channel events on tenant.<t>.telephony.event.<call_id>.<verb>.
//
// run() is the composition root. It mirrors cmd/api's shape:
//
//  1. Load *config.Snapshot.
//  2. Build *zap.Logger with PII redaction.
//  3. Initialise the OTel TracerProvider (non-blocking gRPC client).
//  4. Build the Prometheus *Metrics registry.
//  5. Connect to NATS (RetryOnFailedConnect — boot completes even when the
//     NATS cluster is briefly unreachable).
//  6. Open a *redis.Client.
//  7. Validate cfg.Telephony.Bridge.FSNodes is non-empty.
//  8. Construct *pool.ESLPool, *router.Router, *nats_bridge.Bridge.
//  9. Start two gin engines: :8080 (healthz/readyz) and :9090 (metrics).
//  10. Wait on SIGINT/SIGTERM (or test ctx cancel) and drain in reverse order.
//
// Plan 09 Task 1 ships this file as a real composition root with stub
// telephony subsystems. Tasks 2-6 progressively replace the stubs.
//
// Postgres: Task 1 deliberately does NOT open a *postgres.Pool. Plan 09
// Task 5 (Router) is the first task that needs Postgres for the
// telephony_trunks catalog; opening it here would be premature.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/sociopulse/platform/internal/healthz"
	healthchecks "github.com/sociopulse/platform/internal/healthz/checks"
	"github.com/sociopulse/platform/internal/telephony/nats_bridge"
	"github.com/sociopulse/platform/internal/telephony/pool"
	"github.com/sociopulse/platform/internal/telephony/router"
	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/observability"
)

const (
	// serviceName labels every span, log line, and Prometheus metric this
	// binary emits. It also distinguishes us from cmd/api in shared
	// dashboards / log indices.
	serviceName = "telephony-bridge"

	// defaultConfigDir is the fallback when neither --config-dir nor
	// SOCIOPULSE_CONFIG_DIR is set. In production this is overridden via
	// the k8s ConfigMap mount; in dev the working directory hosts ./configs.
	defaultConfigDir = "configs/development"

	// defaultHealthAddr is where /healthz and /readyz live. Operators
	// expect a static port for kubelet probes; cfg.Observability.Metrics.Bind
	// drives /metrics on a separate listener.
	defaultHealthAddr = ":8080"

	// readinessTimeout is the deadline applied to the parallel /readyz
	// probe. Short enough to fail fast under kubelet's default probe budget
	// (kept identical to cmd/api for operator consistency).
	readinessTimeout = 2 * time.Second

	// metricsShutdownGrace is the deadline given to the metrics listener
	// during shutdown. Local-only and lighter than the public listener.
	metricsShutdownGrace = 5 * time.Second
)

func main() {
	if err := run(rootContext(), runOptions{
		ConfigDir:  parseConfigDir(os.Args[1:]),
		HealthAddr: defaultHealthAddr,
	}); err != nil {
		// Logger may not be initialised yet on early failure; fall back to
		// stderr so process supervisors (k8s, systemd) can capture the cause.
		_, _ = fmt.Fprintf(os.Stderr, "cmd/telephony-bridge: %v\n", err)
		os.Exit(1)
	}
}

// rootContext returns a context cancelled on SIGINT or SIGTERM. Splitting it
// out makes it easy to swap a plain context.Background() in tests via the
// run() seam. The stop func from NotifyContext is intentionally discarded —
// process exit reclaims the registration.
func rootContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	return ctx
}

// parseConfigDir extracts --config-dir from argv with environment fallback.
// Mirrors cmd/api so operators see the same CLI in both binaries.
func parseConfigDir(args []string) string {
	fs := flag.NewFlagSet("cmd/telephony-bridge", flag.ExitOnError)
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

// runOptions bundles the parameters that vary between production main() and
// tests. Tests pick free ports for HealthAddr; production uses the constant.
type runOptions struct {
	// ConfigDir points at the directory containing config.yaml. The
	// snapshot loader appends "config.yaml" itself.
	ConfigDir string

	// HealthAddr is where the gin engine serves /healthz + /readyz. Tests
	// override this with a free 127.0.0.1:N to avoid port collisions when
	// run in parallel.
	HealthAddr string
}

// run is the composition root. It returns nil on graceful shutdown, error
// otherwise. ctx is cancelled by SIGINT/SIGTERM in production and by the
// test driver in cmd/telephony-bridge/main_test.go.
//
//nolint:gocognit,gocyclo,cyclop // composition root is intentionally linear — splitting it further obscures the boot sequence
func run(ctx context.Context, opts runOptions) error {
	// 1. Configuration. Hot-reload mirrors cmd/api so operators can edit
	//    yaml in place — though the bridge consumes far fewer fields, so
	//    most edits are no-ops here.
	snap, err := config.Load(config.LoadOptions{Dir: opts.ConfigDir, HotReload: true})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	defer func() { _ = snap.Close() }()
	cfg := snap.Get()

	// 2. Logger. Same redacting encoder cmd/api uses; the bridge name is
	//    set via cfg.Service.Name in the yaml.
	logger, err := observability.NewLogger(cfg)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()
	logger = logger.Named(serviceName)
	logger.Info("cmd/telephony-bridge starting",
		zap.String("config_dir", opts.ConfigDir),
		zap.String("health_addr", opts.HealthAddr),
		zap.String("metrics_bind", cfg.Observability.Metrics.Bind),
	)

	// 3. Tracer. Non-blocking gRPC client — missing collector does not
	//    block startup; spans simply fail to export and surface in batch
	//    processor logs.
	tracer, tracerShutdown, err := observability.NewTracer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("init tracer: %w", err)
	}
	defer func() { //nolint:contextcheck // detached ctx is intentional for graceful drain after parent ctx cancel
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.GracePeriod)
		defer cancel()
		if err := tracerShutdown(shutdownCtx); err != nil {
			logger.Warn("tracer shutdown", zap.Error(err))
		}
	}()

	// 4. Metrics registry. Cross-cutting Go runtime + process collectors
	//    plus the same HTTP-edge metrics cmd/api exports — the bridge's
	//    health and metrics endpoints get observed too.
	metrics := observability.NewMetrics(cfg)

	// 5. NATS connection. RetryOnFailedConnect lets the bridge boot even
	//    when the NATS cluster is briefly unreachable; readyz reports the
	//    actual state on every probe.
	nc, err := nats.Connect(joinURLs(cfg.NATS.URLs),
		nats.Name(serviceName),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("nats disconnected", zap.Error(err))
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			logger.Info("nats reconnected", zap.String("url", c.ConnectedUrl()))
		}),
		nats.ClosedHandler(func(c *nats.Conn) {
			logger.Info("nats closed", zap.String("last_err", errString(c.LastError())))
		}),
	)
	if err != nil {
		return fmt.Errorf("connect nats: %w", err)
	}
	defer func() {
		// Drain finishes in-flight messages before close; preferred over
		// nc.Close() at SIGTERM. If Drain fails (already-closed, etc.),
		// log and move on — Close is idempotent.
		if err := nc.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			logger.Warn("nats drain", zap.Error(err))
		}
	}()

	// 6. Redis client. ParseURL accepts both redis:// and rediss:// — the
	//    DSN is built from cfg.Database.Redis.{Addr,Password,DB} so the
	//    same yaml drives both binaries.
	rdb, err := openRedis(cfg)
	if err != nil {
		return fmt.Errorf("open redis: %w", err)
	}
	defer func() { _ = rdb.Close() }()

	// 7. Telephony config validation. An empty FSNodes list is operator
	//    error: the bridge cannot do anything useful without at least one
	//    FreeSWITCH endpoint. Failing here is the cheapest place to tell
	//    the operator "your Helm values are wrong".
	fsNodes := nodeAddrs(cfg.Telephony.Bridge.FSNodes)
	if len(fsNodes) == 0 {
		return errors.New("telephony.bridge.fs_nodes must list at least one FreeSWITCH ESL endpoint")
	}

	// 8. Telephony subsystems — Task 4 (pool) is real; Tasks 5/6 still
	//    fill the router + nats_bridge bodies.
	//
	//    HealthcheckInterval comes from cfg.Telephony.Bridge — operators
	//    tune it per-environment via Helm values. Zero falls back to the
	//    pool's 5s default (declared in internal/telephony/pool/pool.go).
	//
	//    Metrics are registered against the shared *prometheus.Registry
	//    so /metrics exposes both the cross-cutting collectors and the
	//    telephony-pool series under the same scrape.
	poolMetrics := pool.RegisterMetrics(metrics.Registry)
	eslPool, err := pool.New(ctx, pool.Config{
		Nodes:          fsNodes,
		HealthInterval: cfg.Telephony.Bridge.HealthcheckInterval,
		Logger:         logger.Named("pool"),
		Metrics:        poolMetrics,
	})
	if err != nil {
		return fmt.Errorf("init esl pool: %w", err)
	}
	defer func() { _ = eslPool.Close() }()

	rt := router.New(router.Config{
		Pool:            eslPool,
		Redis:           rdb,
		BackpressureCap: cfg.Telephony.Bridge.MaxConcurrentPerNode,
		PostgresDSN:     cfg.Database.Postgres.DSN,
		Logger:          logger.Named("router"),
	})
	if err := rt.Start(ctx); err != nil {
		return fmt.Errorf("start router: %w", err)
	}
	defer rt.Stop()

	bridge := nats_bridge.New(nats_bridge.Config{
		NATS:   nc,
		Pool:   eslPool,
		Router: rt,
		Redis:  rdb,
		Logger: logger.Named("nats_bridge"),
	})
	if err := bridge.Start(ctx); err != nil {
		return fmt.Errorf("start nats bridge: %w", err)
	}
	// Stop is the abrupt path; Drain is the graceful path. The shutdown
	// handler (below) calls Drain before returning, after which Stop is a
	// no-op safety net.
	defer bridge.Stop()

	// 9. HTTP servers. Two gin engines so /metrics scrape traffic stays
	//    isolated from /healthz public probes (matches cmd/api's split).
	checks := []healthz.Checker{
		healthchecks.NATSCheck{Conn: natsConnAdapter{nc}},
		healthchecks.RedisCheck{Client: redisPinger{rdb}},
		eslPoolCheck{pool: eslPool},
	}
	healthSrv := buildHealthServer(opts.HealthAddr, cfg, logger, tracer, metrics, checks) //nolint:contextcheck // server inherits ctx via gin handlers, not caller
	metricsSrv := buildMetricsServer(cfg, metrics)

	// 10. errgroup orchestration. Same shape as cmd/api: each long-running
	//     goroutine returns from Listen when the parent ctx cancels.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		logger.Info("health listener up", zap.String("bind", opts.HealthAddr))
		if err := listenAndServe(healthSrv); err != nil {
			return fmt.Errorf("health listen: %w", err)
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

	logger.Info("telephony-bridge started",
		zap.Strings("fs_nodes", fsNodes),
		zap.Int("max_concurrent", cfg.Telephony.Bridge.MaxConcurrentPerNode),
	)

	// 11. Wait for shutdown signal. Reverse order:
	//      a) shut HTTP servers (stop accepting probes / scrapes)
	//      b) drain bridge subscriptions
	//      c) stop router refresh loop (defer above)
	//      d) close pool (defer above)
	//      e) drain NATS (defer above)
	//      f) close Redis (defer above)
	g.Go(func() error {
		<-gctx.Done()
		logger.Info("shutdown signal received, draining listeners")

		// Detached contexts — by the time this defer runs, the parent ctx
		// is already cancelled, so we deliberately do NOT inherit from it.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.GracePeriod)
		defer cancel()
		//nolint:contextcheck // detached ctx is intentional for graceful drain
		shutdownServer(shutdownCtx, healthSrv, "health", logger)

		mctx, mcancel := context.WithTimeout(context.Background(), metricsShutdownGrace)
		defer mcancel()
		//nolint:contextcheck // detached ctx is intentional for graceful drain
		shutdownServer(mctx, metricsSrv, "metrics", logger)

		// Drain the bridge with the same grace budget. Tasks 2/3 fill in
		// the real subscription cleanup; the skeleton no-ops.
		bctx, bcancel := context.WithTimeout(context.Background(), cfg.Shutdown.GracePeriod)
		defer bcancel()
		//nolint:contextcheck // detached ctx is intentional for graceful drain
		if err := bridge.Drain(bctx); err != nil {
			logger.Warn("bridge drain", zap.Error(err))
		}
		return nil
	})

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("server group: %w", err)
	}
	logger.Info("cmd/telephony-bridge shutdown complete")
	return nil
}

// joinURLs flattens cfg.NATS.URLs into the comma-separated form
// nats.Connect understands. Empty list yields nats.DefaultURL so the boot
// path remains usable in dev tests.
func joinURLs(urls []string) string {
	if len(urls) == 0 {
		return nats.DefaultURL
	}
	return strings.Join(urls, ",")
}

// nodeAddrs flattens config.FSNode -> ESLEndpoint slice.
func nodeAddrs(nodes []config.FSNode) []string {
	if len(nodes) == 0 {
		return nil
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.ESLEndpoint != "" {
			out = append(out, n.ESLEndpoint)
		}
	}
	return out
}

// openRedis builds a *redis.Client from cfg.Database.Redis. Mirrors the
// shape used by cmd/api / module Register paths, just inlined here because
// the bridge has no Deps struct.
func openRedis(cfg config.Config) (*redis.Client, error) {
	if cfg.Database.Redis.Addr == "" {
		return nil, errors.New("database.redis.addr required")
	}
	return redis.NewClient(&redis.Options{
		Addr:     cfg.Database.Redis.Addr,
		Password: cfg.Database.Redis.Password,
		DB:       cfg.Database.Redis.DB,
		PoolSize: cfg.Database.Redis.PoolSize,
	}), nil
}

// errString safely stringifies an optional error for log fields. The NATS
// callbacks pass nil sometimes; zap.Error(nil) is fine but the explicit
// helper makes the closure intent clear.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

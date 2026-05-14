// Package main is the entrypoint for cmd/worker — the СоциоПульс
// background worker process. Today it owns four errgroup-driven
// daemons:
//
//   - dialer.retry.Orchestrator (Plan 10 Task 10) — leader-elected
//     retry pipeline that drains failed dialer calls back into the queue.
//   - recording.RetentionPass (Plan 12.4 Task 2 + Task 5) — leader-
//     elected sweep that flips committed → cold and cold → deleted on
//     the per-tenant retention schedule.
//   - recording.IntegrityPass (Plan 12.4 Task 3 + Task 5) — leader-
//     elected sweep that 1%-samples committed recordings and recomputes
//     sha256 against ciphertext to catch silent corruption.
//   - analytics.IngestPipeline (Plan 13.2 Task 6) — drains the three
//     analytics-bound NATS subjects (calls / operator_state /
//     recording.uploaded) into ClickHouse via per-subject batched
//     inserts with dedup-LRU. Best-effort: NATS or CH unreachable at
//     boot → WARN + skip; the other daemons keep running.
//
// Composition root:
//
//  1. Load configuration (pkg/config).
//  2. Set up logger + metrics (pkg/observability).
//  3. Open Postgres pool, Redis client.
//  4. Build the dialer retry orchestrator with explicit deps. We do
//     NOT go through dialer.Module.Register here — the worker only
//     needs the retry pipeline and pulling the rest of the dialer
//     stack would also start the heartbeat watchdog (a cmd/api
//     responsibility). Building inline keeps the lifecycle obvious.
//  5. Build the recording retention + integrity passes when
//     recording.enabled=true AND wire.LocalPorts validates. Empty /
//     invalid LocalKEKs WARN + skip — the dialer retry orchestrator
//     keeps running. Each pass gets its own *retry.PgLeader against a
//     distinct advisory-lock key (worker.RetentionLockKey /
//     IntegrityLockKey) so retention and integrity can lead
//     simultaneously.
//  6. errgroup orchestrate:
//     - retry.Orchestrator.Run
//     - recording.RetentionPass.Run (when enabled)
//     - recording.IntegrityPass.Run (when enabled)
//     - /healthz HTTP listener for k8s readiness
//     - SIGINT/SIGTERM signal handler that cancels the parent ctx.
//  7. Block until ctx done; tear down.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/sociopulse/platform/internal/dialer/queue"
	"github.com/sociopulse/platform/internal/dialer/retry"
	rmetrics "github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/service"
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/internal/recording/wire"
	"github.com/sociopulse/platform/internal/recording/worker"
	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/observability"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

const (
	// defaultConfigDir mirrors cmd/api so a single SOCIOPULSE_CONFIG_DIR
	// env var (or shared k8s ConfigMap mount) feeds both binaries.
	defaultConfigDir = "configs/development"

	// defaultHealthBind is the bind address for the /healthz endpoint
	// used by k8s readiness probes. Falls back when ws.bind / http.bind
	// are missing from config.
	defaultHealthBind = ":9090"

	// healthShutdownGrace caps the /healthz listener drain on graceful
	// shutdown. Short — the listener has no in-flight long-running
	// requests by design.
	healthShutdownGrace = 2 * time.Second
)

func main() {
	if err := run(rootContext(), parseConfigDir(os.Args[1:])); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "cmd/worker: %v\n", err)
		os.Exit(1)
	}
}

// rootContext returns a context cancelled on SIGINT or SIGTERM.
// Splitting it out makes the test driver easy: tests pass a plain
// context.Background() through run() directly.
func rootContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	return ctx
}

// parseConfigDir extracts --config-dir from argv with environment
// fallback. Mirrors cmd/api's helper of the same name.
func parseConfigDir(args []string) string {
	fs := flag.NewFlagSet("cmd/worker", flag.ExitOnError)
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

// run is the composition root. It returns nil on graceful shutdown,
// error otherwise. ctx is cancelled by SIGINT/SIGTERM in production
// and by the test driver in cmd/worker/main_test.go.
//
//nolint:gocognit,gocyclo // composition root is intentionally linear
func run(ctx context.Context, configDir string) error {
	// 1. Load configuration. Hot-reload is disabled in the worker —
	//    nothing in the orchestrator path consumes hot-reloaded values
	//    today, and a long-running PG advisory lock means the
	//    worker survives a config change without restart.
	snap, err := config.Load(config.LoadOptions{Dir: configDir, HotReload: false})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	defer func() { _ = snap.Close() }()
	cfg := snap.Get()

	// 2. Logger.
	logger, err := observability.NewLogger(cfg)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()
	logger.Info("cmd/worker starting", zap.String("config_dir", configDir))

	// 3. Postgres + Redis.
	pool, pingErr := openPool(ctx, cfg, logger)
	if pool != nil {
		defer pool.Close()
	}
	if pingErr != nil {
		// Hard error: the orchestrator is the worker's only job today.
		// Without PG it has nothing to do.
		return fmt.Errorf("postgres unavailable: %w", pingErr)
	}

	rdb, redisErr := openRedis(ctx, cfg, logger)
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
	}
	if redisErr != nil {
		return fmt.Errorf("redis unavailable: %w", redisErr)
	}

	// 4. Dialer retry orchestrator. The worker explicitly builds the
	//    pieces — Leader, Reader, Decryptor, Queue — rather than going
	//    through dialer.Module.Register. The module's Register also
	//    starts the heartbeat watchdog, which is cmd/api's
	//    responsibility (cmd/api owns the operator hashes; cmd/worker
	//    is the retry sweep).
	leader, err := retry.NewPgLeader(pool, 0, logger.Named("retry.leader"))
	if err != nil {
		return fmt.Errorf("build retry leader: %w", err)
	}
	reader, err := retry.NewPgReader(pool)
	if err != nil {
		return fmt.Errorf("build retry reader: %w", err)
	}
	q, err := queue.New(queue.Config{
		Redis:  rdb,
		Logger: logger.Named("queue"),
	})
	if err != nil {
		return fmt.Errorf("build queue: %w", err)
	}
	// Decryptor: the worker has no direct KMS dependency in v1. The
	// passthrough surface returns ciphertext bytes verbatim, which
	// matches dev-env phone columns stored as plaintext. Plan 12 wires
	// the real KMS-backed decryptor — until then, an integration
	// environment that needs real encryption must run cmd/api's full
	// dialer module instead of cmd/worker.
	orch, err := retry.New(retry.Config{
		Leader:    leader,
		Reader:    reader,
		Decryptor: passthroughDecryptor{},
		Queue:     q,
		Logger:    logger.Named("retry"),
	})
	if err != nil {
		return fmt.Errorf("build retry orchestrator: %w", err)
	}

	// 4b. Recording workers — Plan 12.4 Task 5. Built only when the
	//     recording module is enabled AND wire.LocalPorts validates.
	//     Empty / invalid KEKs WARN + skip; the dialer retry pipeline
	//     keeps running. Returned runners are appended to the errgroup
	//     below alongside the dialer Orchestrator.Run.
	recordingRunners, err := buildRecordingWorkers(cfg, pool, logger)
	if err != nil {
		return fmt.Errorf("build recording workers: %w", err)
	}

	// 4c. NATS subscriber — Plan 13.2 Task 6. Required by the
	//     analytics ingest pipeline. Best-effort (mirrors cmd/api): a
	//     connection failure logs a WARN and analytics ingest is
	//     skipped. The other daemons keep running. The publisher is
	//     returned for symmetry but is currently unused.
	natsPub, natsSub, natsErr := openNATS(ctx, cfg, logger)
	if natsErr != nil {
		logger.Warn("nats unreachable; analytics ingest will be skipped",
			zap.Strings("urls", redactNATSURLs(cfg.NATS.URLs)),
			zap.Error(natsErr),
		)
	} else {
		logger.Info("nats publisher + subscriber up",
			zap.Strings("urls", redactNATSURLs(cfg.NATS.URLs)),
		)
	}
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

	// 4d. Analytics ingest pipeline — Plan 13.2 Task 6. Gated on
	//     cfg.Analytics.Enabled && natsSub != nil && CH reachable.
	//     Degraded boot is the rule: a return of (nil, nil) is the
	//     happy "ingest disabled" path; the dialer retry + recording
	//     sweeps continue. A non-nil error surfaces only on
	//     configuration mistakes (bad validation) and trips fail-fast.
	//
	//     Subscriber is interface-typed (eventbus.Subscriber), so we
	//     pass natsSub directly. nil-passing through the helper is
	//     fine — it short-circuits with a WARN.
	var natsSubIface eventbus.Subscriber
	if natsSub != nil {
		natsSubIface = natsSub
	}
	analyticsRunner, err := buildAnalyticsIngest(ctx, cfg, natsSubIface, logger.Named("analytics"))
	if err != nil {
		return fmt.Errorf("build analytics ingest: %w", err)
	}
	defer analyticsRunner.Close(logger)

	// 5. /healthz listener. k8s readiness probes hit this; we keep
	//    the surface tiny (just liveness) because the orchestrator's
	//    own metrics dashboard is the readiness source of truth for
	//    "is the worker actually doing useful work".
	healthSrv := buildHealthServer(cfg)

	// 6. errgroup orchestration. Each long-running goroutine returns
	//    when the parent ctx is cancelled.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		logger.Info("retry orchestrator running",
			zap.String("desc", orch.String()))
		if err := orch.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("retry orchestrator: %w", err)
		}
		return nil
	})
	for _, runner := range recordingRunners {
		// Per-iteration loop-var capture is automatic on Go 1.22+ (the
		// project pins 1.26.3 in CI). The closure below captures
		// `runner` without an explicit `runner := runner` shadow.
		g.Go(func() error {
			if err := runner.run(gctx); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("%s: %w", runner.name, err)
			}
			return nil
		})
	}
	if analyticsRunner != nil {
		g.Go(func() error {
			return analyticsRunner.run(gctx, logger.Named("analytics"))
		})
	}
	g.Go(func() error {
		logger.Info("/healthz listener up", zap.String("bind", healthSrv.Addr))
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("health listener: %w", err)
		}
		return nil
	})

	// 7. Wait for shutdown signal.
	g.Go(func() error {
		<-gctx.Done()
		logger.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), healthShutdownGrace)
		defer cancel()
		//nolint:contextcheck // detached ctx is intentional for graceful drain after parent ctx cancel
		if err := healthSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("health server shutdown", zap.Error(err))
		}
		return nil
	})

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("worker group: %w", err)
	}
	logger.Info("cmd/worker shutdown complete")
	return nil
}

// recordingRunner pairs a Run-method with a name so the errgroup wrap
// can log a meaningful identifier when the runner errors. The struct
// stays unexported because every consumer lives inside cmd/worker.
type recordingRunner struct {
	name string
	run  func(context.Context) error
}

// buildRecordingWorkers wires the retention + integrity passes when
// recording.enabled=true. Returns an empty slice (no error) when
// disabled OR when LocalKEKs validates to nil — in those modes
// cmd/worker keeps running the dialer retry orchestrator only.
//
// A non-nil error is reserved for explicit configuration mistakes
// (bad hex, wrong KEK length, validation failures from the worker
// constructors). Boot fails-fast on those rather than silently
// degrading: the operator should fix the config rather than wonder why
// the integrity pass never logs a sweep.
//
// The Prometheus collectors register against a fresh
// prometheus.NewRegistry — cmd/worker has no /metrics endpoint today
// (Plan 12 Task 6 will add one), so the registry is private and only
// the gauge-set's correctness is exercised. When the metrics endpoint
// lands, swap this for observability.NewMetrics(cfg).Registry.
func buildRecordingWorkers(
	cfg config.Config,
	pool *postgres.Pool,
	logger *zap.Logger,
) ([]recordingRunner, error) {
	if !cfg.Recording.Enabled {
		logger.Info("recording workers disabled (recording.enabled=false)")
		return nil, nil
	}

	rlog := logger.Named("recording")
	ports, err := wire.LocalPorts(cfg.Recording, rlog)
	if err != nil {
		return nil, fmt.Errorf("local ports: %w", err)
	}
	if ports == nil {
		// LocalPorts already logged a WARN — recording workers can't
		// run without DEK unwrap (integrity pass) or object storage
		// (retention hard-delete). Skip registration and let the
		// dialer-retry orchestrator continue.
		rlog.Warn("recording workers skipped — wire.LocalPorts returned nil (empty/invalid local_keks)")
		return nil, nil
	}

	// Shared metrics registry — both passes register against the same
	// *RecordingMetrics and the LeaderActive gauge's `pass` label
	// disambiguates them. The registry is intentionally private until
	// cmd/worker grows a /metrics endpoint.
	metrics, err := rmetrics.RegisterRecordingMetrics(prometheus.NewRegistry())
	if err != nil {
		return nil, fmt.Errorf("recording metrics: %w", err)
	}

	pgStore := store.NewPostgresStore(pool)
	outboxWriter := outbox.NewPostgresWriter()

	// Recording service — needed only by the integrity pass for
	// VerifyChecksum. Mirrors internal/recording/module.go's Module.Register
	// shape but without the locator + HTTP transport (cmd/worker doesn't
	// host any HTTP routes).
	svc := service.New(service.Deps{
		Pool:    pool,
		Store:   pgStore,
		Outbox:  outboxWriter,
		Logger:  rlog.Named("service"),
		Metrics: metrics,
		KMS:     ports.DEK,
		Objects: ports.Objects,
	})

	// Leader-election: each pass gets its own advisory-lock slot so
	// retention and integrity can lead simultaneously. The hash keys
	// are computed from distinct seed strings (recording.retention_pass
	// vs recording.integrity_pass), so they don't collide with each
	// other or with the dialer's DefaultLockKey.
	retLeader, err := retry.NewPgLeader(pool, worker.RetentionLockKey, rlog.Named("retention.leader"))
	if err != nil {
		return nil, fmt.Errorf("retention leader: %w", err)
	}
	intLeader, err := retry.NewPgLeader(pool, worker.IntegrityLockKey, rlog.Named("integrity.leader"))
	if err != nil {
		return nil, fmt.Errorf("integrity leader: %w", err)
	}

	wcfg := cfg.Recording.Workers
	rp, err := worker.NewRetentionPass(worker.RetentionConfig{
		Pool:     pool,
		Leader:   retLeader,
		Store:    pgStore,
		Objects:  ports.Objects,
		Outbox:   outboxWriter,
		Metrics:  metrics,
		Logger:   rlog.Named("retention"),
		Interval: wcfg.RetentionInterval,
		Batch:    wcfg.RetentionBatch,
	})
	if err != nil {
		return nil, fmt.Errorf("build retention pass: %w", err)
	}
	ip, err := worker.NewIntegrityPass(worker.IntegrityConfig{
		Pool:          pool,
		Leader:        intLeader,
		Store:         pgStore,
		Service:       svc,
		Metrics:       metrics,
		Logger:        rlog.Named("integrity"),
		Interval:      wcfg.IntegrityInterval,
		Batch:         wcfg.IntegrityBatch,
		SamplePercent: wcfg.IntegritySamplePercent,
	})
	if err != nil {
		return nil, fmt.Errorf("build integrity pass: %w", err)
	}

	rlog.Info("recording workers registered",
		zap.Duration("retention_interval", wcfg.RetentionInterval),
		zap.Int("retention_batch", wcfg.RetentionBatch),
		zap.Duration("integrity_interval", wcfg.IntegrityInterval),
		zap.Int("integrity_batch", wcfg.IntegrityBatch),
		zap.Float64("integrity_sample_percent", wcfg.IntegritySamplePercent),
	)
	return []recordingRunner{
		{name: "recording.retention", run: rp.Run},
		{name: "recording.integrity", run: ip.Run},
	}, nil
}

// buildHealthServer returns an *http.Server exposing /healthz on the
// configured bind address. Falls back to defaultHealthBind when no
// dedicated worker bind is configured.
func buildHealthServer(cfg config.Config) *http.Server {
	bind := cfg.Observability.Metrics.Bind
	if bind == "" {
		bind = defaultHealthBind
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return &http.Server{
		Addr:              bind,
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

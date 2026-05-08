// Package main is the entrypoint for cmd/worker — the СоциоПульс
// background worker process. Today (Plan 10 Task 10) it owns the
// dialer retry orchestrator's leader-election loop; future plans will
// add scheduled aggregations, cold-tier archival, and asynq workers
// for asynchronous CRM imports.
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
//  5. errgroup orchestrate:
//     - retry.Orchestrator.Run
//     - /healthz HTTP listener for k8s readiness
//     - SIGINT/SIGTERM signal handler that cancels the parent ctx.
//  6. Block until ctx done; tear down.
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

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/sociopulse/platform/internal/dialer/queue"
	"github.com/sociopulse/platform/internal/dialer/retry"
	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/observability"
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
//nolint:gocognit // composition root is intentionally linear
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

package main

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
	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// analyticsBoot is the lifecycle wrapper for the IngestPipeline plus
// its private dependencies. Returning a single struct lets the caller
// defer one Close + append one Run-loop without re-deriving the wiring
// at the call site.
//
// chConn is owned by analyticsBoot — Close drains it.
type analyticsBoot struct {
	pipeline *service.IngestPipeline
	chConn   *store.Conn
}

// Close drains the ClickHouse connection. The pipeline itself has no
// Close — Run returns when ctx is cancelled and the goroutine exits.
// Idempotent; nil-safe.
func (a *analyticsBoot) Close(logger *zap.Logger) {
	if a == nil || a.chConn == nil {
		return
	}
	if err := a.chConn.Close(); err != nil {
		logger.Warn("analytics: clickhouse close failed", zap.Error(err))
	}
}

// buildAnalyticsIngest wires the analytics ingest pipeline against the
// supplied subscriber and configured ClickHouse DSN. Returns a non-nil
// *analyticsBoot when the pipeline is constructed; nil + nil error
// when the path is degraded (analytics disabled, NATS unreachable, CH
// unreachable, empty DSN). A non-nil error surfaces only on
// configuration mistakes or explicit construction failures (bad config
// validation, metric-registry collisions) — those fail boot fast
// rather than silently skipping.
//
// Degraded-boot matrix:
//   - cfg.Analytics.Enabled == false  → nil + INFO log
//   - cfg.Database.ClickHouse.DSN == "" → nil + WARN log
//   - natsSub == nil → nil + WARN log (caller already logged the NATS
//     unreachability separately)
//   - CH Open fails  → nil + WARN log; dialer retry + recording sweeps
//     continue
//
// Metrics: the analytics ingest collectors are registered against a
// fresh prometheus.NewRegistry. cmd/worker has no /metrics endpoint
// today (Plan 12 Task 6 will add one); when it does, swap the private
// registry for observability.NewMetrics(cfg).Registry. The same
// pattern is used by buildRecordingWorkers above.
func buildAnalyticsIngest(
	ctx context.Context,
	cfg config.Config,
	natsSub eventbus.Subscriber,
	logger *zap.Logger,
) (*analyticsBoot, error) {
	if !cfg.Analytics.Enabled {
		logger.Info("analytics ingest disabled (analytics.enabled=false)")
		return nil, nil
	}
	if cfg.Database.ClickHouse.DSN == "" {
		logger.Warn("analytics ingest skipped: clickhouse DSN empty")
		return nil, nil
	}
	if natsSub == nil {
		logger.Warn("analytics ingest skipped: NATS subscriber unavailable")
		return nil, nil
	}
	// Surface config mistakes at boot rather than at first message.
	if err := cfg.Analytics.Validate(); err != nil {
		return nil, fmt.Errorf("cmd/worker: analytics config: %w", err)
	}

	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second) //nolint:mnd // mirrors internal/analytics/module.go::openCH dial timeout
	defer cancel()
	chConn, err := store.Open(openCtx, store.Config{
		DSN:           cfg.Database.ClickHouse.DSN,
		BatchSize:     cfg.Analytics.BatchSize,
		FlushInterval: cfg.Analytics.FlushInterval,
		DialTimeout:   5 * time.Second, //nolint:mnd // mirrors internal/analytics/module.go::openCH dial timeout
		Logger:        logger.Named("store"),
	})
	if err != nil {
		// Degraded boot: CH unreachable at startup → log + skip.
		// dialer retry + recording sweeps keep running.
		logger.Warn("analytics ingest skipped: clickhouse open failed", zap.Error(err))
		return nil, nil
	}

	// Private registry — cmd/worker has no /metrics endpoint yet (see
	// the buildRecordingWorkers note). RegisterIngestMetrics returns
	// an error on duplicate registration, which can never happen
	// against a fresh registry; treating it as a hard failure keeps
	// the wiring contract honest.
	ingestMetrics, err := metrics.RegisterIngestMetrics(prometheus.NewRegistry())
	if err != nil {
		_ = chConn.Close()
		return nil, fmt.Errorf("cmd/worker: analytics ingest metrics: %w", err)
	}

	pipeline, err := service.NewIngestPipeline(
		natsSub,
		&service.StoreAdapter{Conn: chConn},
		logger.Named("ingest"),
		ingestMetrics,
		service.IngestConfig{
			BatchSize:     cfg.Analytics.BatchSize,
			FlushInterval: cfg.Analytics.FlushInterval,
			DedupSize:     cfg.Analytics.DedupLRUSize,
			QueueGroup:    cfg.Analytics.QueueGroup,
			DrainTimeout:  cfg.Analytics.DrainTimeout,
		},
	)
	if err != nil {
		_ = chConn.Close()
		return nil, fmt.Errorf("cmd/worker: build analytics ingest pipeline: %w", err)
	}

	logger.Info("analytics ingest pipeline wired",
		zap.Int("batch_size", cfg.Analytics.BatchSize),
		zap.Duration("flush_interval", cfg.Analytics.FlushInterval),
		zap.Int("dedup_lru_size", cfg.Analytics.DedupLRUSize),
		zap.String("queue_group", cfg.Analytics.QueueGroup),
	)
	return &analyticsBoot{pipeline: pipeline, chConn: chConn}, nil
}

// runAnalyticsIngest is the errgroup goroutine body for the ingest
// pipeline. Tolerates a nil receiver so the call site does not need
// a guard around g.Go. Returns nil on a clean ctx.Done shutdown so the
// errgroup doesn't trip on context.Canceled.
func (a *analyticsBoot) run(ctx context.Context, logger *zap.Logger) error {
	if a == nil || a.pipeline == nil {
		return nil
	}
	logger.Info("analytics ingest pipeline running")
	if err := a.pipeline.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("analytics ingest: %w", err)
	}
	return nil
}

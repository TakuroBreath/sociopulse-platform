package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	anametrics "github.com/sociopulse/platform/internal/analytics/metrics"
	anaservice "github.com/sociopulse/platform/internal/analytics/service"
	anastore "github.com/sociopulse/platform/internal/analytics/store"
	storage "github.com/sociopulse/platform/internal/recording/storage"
	recwire "github.com/sociopulse/platform/internal/recording/wire"
	rptevents "github.com/sociopulse/platform/internal/reports/events"
	rptservice "github.com/sociopulse/platform/internal/reports/service"
	reportstore "github.com/sociopulse/platform/internal/reports/store"
	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"

	"github.com/prometheus/client_golang/prometheus"
)

// reportsBoot owns the reports asynq.Server + Consumer lifecycle in
// cmd/worker. Mirrors analyticsBoot's shape: Close drains owned
// resources (the analytics CH conn the worker built standalone for
// the renderers); run is the errgroup body.
//
// cmd/worker constructs reports' renderer-side dependencies INLINE
// rather than going through reports.Module.Register — Register is
// cmd/api's path (HTTP routes + Queue), and cmd/worker only needs the
// Consumer-side compute path. The analytics.ServiceRO the Consumer
// passes to its render dispatcher is a worker-private QueryService
// over a worker-private ClickHouse connection, isolated from any
// cmd/api side QueryService so a CH outage in one binary doesn't take
// the other down.
type reportsBoot struct {
	consumer *rptservice.Consumer
	chConn   *anastore.Conn
}

// Close drains the worker-owned ClickHouse connection. The Consumer
// itself has no Close — Run returns when ctx is cancelled and the
// goroutine exits. Idempotent; nil-safe.
func (r *reportsBoot) Close(logger *zap.Logger) {
	if r == nil || r.chConn == nil {
		return
	}
	if err := r.chConn.Close(); err != nil {
		logger.Warn("reports: clickhouse close failed", zap.Error(err))
	}
}

// buildReportsBoot wires the reports Consumer against the supplied
// pool + redis + analytics config. Degraded-boot matrix mirrors
// analyticsBoot:
//
//   - cfg.Redis.Addr == ""              → nil + INFO  (no Redis → no asynq)
//   - cfg.Reports.* validation fails    → nil + WARN
//   - cfg.Recording.local_keks empty    → nil + WARN  (no ObjectStore →
//     nowhere to upload)
//   - cfg.Database.ClickHouse.DSN == "" → nil + WARN  (no CH → no
//     renderable data)
//   - ClickHouse Open fails             → nil + WARN
//   - cfg.S3.Buckets.Reports == ""      → nil + WARN
//
// A non-nil error surfaces ONLY on configuration mistakes that the
// operator must fix (analytics.Validate failure, metrics-registry
// collision). Boot fails-fast on those rather than silently degrading.
//
//nolint:gocognit,gocyclo // composition-root style: linear degraded-boot checks
func buildReportsBoot(
	ctx context.Context,
	cfg config.Config,
	pool *postgres.Pool,
	rdb redis.UniversalClient,
	logger *zap.Logger,
) (*reportsBoot, error) {
	rlog := logger.Named("reports")

	if cfg.Database.Redis.Addr == "" {
		rlog.Info("reports worker: redis disabled, async path skipped")
		return nil, nil
	}
	if pool == nil {
		rlog.Warn("reports worker: postgres pool nil, async path skipped")
		return nil, nil
	}
	if rdb == nil {
		rlog.Warn("reports worker: redis client nil, async path skipped")
		return nil, nil
	}

	// Apply Reports defaults in-place. Config.Validate is the canonical
	// path (cfg passes through pkg/config.Load → Validate), but defence-
	// in-depth: a future test wiring that bypasses config.Load still
	// produces sensible defaults here.
	rptCfg := &cfg.Reports
	_ = rptCfg.Validate()

	bucket := cfg.S3.Buckets.Reports
	if bucket == "" {
		rlog.Warn("reports worker: cfg.S3.Buckets.Reports empty, async path skipped")
		return nil, nil
	}

	// ObjectStore: reuse recording.wire.LocalPorts the same way
	// cmd/api does. nil → no KEKs configured → can't run the
	// upload step; skip with a WARN.
	ports, err := recwire.LocalPorts(cfg.Recording, rlog.Named("recording"))
	if err != nil {
		return nil, fmt.Errorf("reports worker: local recording ports: %w", err)
	}
	var objStore storage.ObjectStore
	if ports != nil {
		objStore = ports.Objects
	}
	if objStore == nil {
		rlog.Warn("reports worker: ObjectStore unavailable (no local_keks?), async path skipped")
		return nil, nil
	}

	// Build analytics ServiceRO inline. Mirrors internal/analytics/module.go
	// but no HTTP routes — the QueryService is consumed by the reports
	// renderer dispatcher inside handleJobRun.
	if !cfg.Analytics.Enabled {
		rlog.Warn("reports worker: analytics disabled in config, async path skipped")
		return nil, nil
	}
	if cfg.Database.ClickHouse.DSN == "" {
		rlog.Warn("reports worker: clickhouse DSN empty, async path skipped")
		return nil, nil
	}

	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second) //nolint:mnd // mirrors analyticsBoot dial timeout
	defer cancel()
	chConn, err := anastore.Open(openCtx, anastore.Config{
		DSN:           cfg.Database.ClickHouse.DSN,
		BatchSize:     1, // unused for the query side — see analytics/module.go::openCH
		FlushInterval: time.Second,
		DialTimeout:   5 * time.Second, //nolint:mnd
		Logger:        rlog.Named("analytics.store"),
	})
	if err != nil {
		rlog.Warn("reports worker: clickhouse open failed, async path skipped", zap.Error(err))
		return nil, nil
	}

	queryMetrics, err := anametrics.RegisterQueryMetrics(prometheus.NewRegistry())
	if err != nil {
		_ = chConn.Close()
		return nil, fmt.Errorf("reports worker: register analytics query metrics: %w", err)
	}

	queryCache := anaservice.NewRedisCache(rdb, rlog.Named("analytics.cache"))

	// crm.ProjectService is not in the worker's locator (the worker has
	// no module.Register sequence). Reports' renderer dispatchers that
	// need region progress fall back to Plan=0 — Q12 documented
	// behaviour. A future plan can wire crm.ProjectService into the
	// worker's locator if needed.
	queryService, err := anaservice.NewQueryService(
		&anaservice.StoreReaderAdapter{Conn: chConn},
		queryCache,
		nil, // crm.ProjectService unwired in cmd/worker — Plan=0 fallback
		rlog.Named("analytics.query"),
		queryMetrics,
		anaservice.QueryConfig{
			CacheShortTTL:       cfg.Analytics.CacheShortTTL,
			CacheLongTTL:        cfg.Analytics.CacheLongTTL,
			LongWindowThreshold: cfg.Analytics.LongWindowThreshold,
		},
	)
	if err != nil {
		_ = chConn.Close()
		return nil, fmt.Errorf("reports worker: build analytics QueryService: %w", err)
	}

	// asynq.Server lifecycle. Concurrency + queue name from cfg.Reports.
	server := asynq.NewServer(
		asynq.RedisClientOpt{
			Addr:     cfg.Database.Redis.Addr,
			Password: cfg.Database.Redis.Password,
			DB:       cfg.Database.Redis.DB,
		},
		asynq.Config{
			Concurrency: rptCfg.AsynqConcurrency,
			Queues:      map[string]int{rptCfg.QueueName: 1},
			Logger:      zapAsynqLogger{l: rlog.Named("asynq")},
		},
	)

	// Build the rest of the reports stack: store + audit + ready
	// publisher are stateless aside from the outbox writer reference,
	// so the worker constructs fresh instances rather than reusing
	// anything from a (non-existent) locator. The Consumer's flip
	// closures default to reportstore.Mark*Tx (see NewConsumer).
	outboxWriter := outbox.NewPostgresWriter()
	store := reportstore.NewPG(pool)
	audit := rptservice.NewAuditEmitter(outboxWriter)
	readyPub := rptevents.NewReportReadyPublisher(outboxWriter)

	consumer := rptservice.NewConsumer(rptservice.ConsumerDeps{
		Server:      server,
		Analytics:   analyticsapi.ServiceRO(queryService),
		Pool:        pool,
		ObjectStore: objStore,
		Audit:       audit,
		ReadyPub:    readyPub,
		Bucket:      bucket,
		PresignTTL:  rptCfg.PresignedURLTTL,
		Logger:      rlog.Named("consumer"),
	})

	// Keep the store reference alive so a future caller (e.g. a worker-
	// side cancel surface) can re-use it. Today the Consumer side reads
	// no rows directly — every state-flip rides through the *Tx free
	// functions baked into NewConsumer.
	_ = store

	rlog.Info("reports worker boot ready",
		zap.String("queue", rptCfg.QueueName),
		zap.Int("concurrency", rptCfg.AsynqConcurrency),
		zap.String("bucket", bucket),
		zap.Duration("presign_ttl", rptCfg.PresignedURLTTL),
	)
	return &reportsBoot{consumer: consumer, chConn: chConn}, nil
}

// run is the errgroup goroutine body for the reports Consumer.
// Tolerates a nil receiver so the call site does not need a guard
// around g.Go. Returns nil on a clean ctx.Done shutdown so the
// errgroup doesn't trip on context.Canceled.
func (r *reportsBoot) run(ctx context.Context, logger *zap.Logger) error {
	if r == nil || r.consumer == nil {
		return nil
	}
	logger.Info("reports consumer running")
	if err := r.consumer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("reports consumer: %w", err)
	}
	return nil
}

// zapAsynqLogger adapts *zap.Logger to asynq's Logger interface (Print-
// style methods). asynq invokes these for queue-server lifecycle events;
// we surface them at the appropriate zap level so they land in the
// project's structured log stream.
//
// Duplicated from internal/crm/module.go::zapAsynqLogger because there's
// no shared package for this two-line adapter; copying the type is
// cheaper than introducing a pkg/asynqlog seam for the cmd/worker side.
type zapAsynqLogger struct{ l *zap.Logger }

func (z zapAsynqLogger) Debug(args ...any) { z.l.Sugar().Debug(args...) }
func (z zapAsynqLogger) Info(args ...any)  { z.l.Sugar().Info(args...) }
func (z zapAsynqLogger) Warn(args ...any)  { z.l.Sugar().Warn(args...) }
func (z zapAsynqLogger) Error(args ...any) { z.l.Sugar().Error(args...) }
func (z zapAsynqLogger) Fatal(args ...any) { z.l.Sugar().Fatal(args...) }

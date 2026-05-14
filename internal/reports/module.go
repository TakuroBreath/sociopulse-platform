// Package reports is the module-registration entry point for the
// reports module (Plan 13.3).
//
// Composition model (parallel to internal/analytics/module.go and
// internal/recording/module.go):
//
//   - cmd/api walks the modules.Registry, calls Module.Register with
//     Deps. Register pulls analytics.ServiceRO + auth.RBACChecker from
//     the locator, builds the per-tenant PG store, constructs the
//     service aggregate (Runner + Audit + ReadyPub), wires the asynq
//     Queue when Redis is available, mounts HTTP routes under
//     /api/reports, and publishes everything cmd/worker needs onto the
//     locator under "reports.*" keys.
//
//   - cmd/worker reads the locator entries published here to construct
//     the asynq.Server + Consumer lifecycle (see cmd/worker/reports.go).
//     The Consumer runs in cmd/worker, NOT cmd/api — cmd/api's role is
//     limited to the HTTP transport + Queue.Enqueue + sync Runner.
//
// Degraded-boot story (mirrors analytics + recording):
//
//   - d.Logger nil           → hard error (composition-root invariant)
//   - d.HTTPRouter nil       → INFO log + skip (worker-only boot)
//   - d.Pool nil             → INFO log + skip (no DB, nothing to wire)
//   - analytics.ServiceRO    → WARN + skip HTTP routes
//     missing from locator
//   - auth.RBACChecker       → WARN + skip HTTP routes
//     missing from locator
//   - d.Redis nil            → WARN + Queue stays nil; sync path
//     still works, async path returns 501-ish
//     via Queue == nil at handler time
//   - cfg.S3.Buckets.Reports → WARN + skip routes (no destination for
//     empty                    artifacts)
//   - cfg.Config nil         → hard error
//
// The module owns no long-running goroutines in cmd/api — Consumer.Run
// lives in cmd/worker. No Start/Stop lifecycle on this side.
package reports

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	analyticsmod "github.com/sociopulse/platform/internal/analytics"
	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	authmod "github.com/sociopulse/platform/internal/auth"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/internal/modules"
	storage "github.com/sociopulse/platform/internal/recording/storage"
	rptevents "github.com/sociopulse/platform/internal/reports/events"
	rptservice "github.com/sociopulse/platform/internal/reports/service"
	reportstore "github.com/sociopulse/platform/internal/reports/store"
	rpthttp "github.com/sociopulse/platform/internal/reports/transport/http"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
	"github.com/sociopulse/platform/pkg/outbox"
)

// Locator keys this module publishes for cmd/worker consumption.
const (
	// LocatorService publishes the *service.Service aggregate (Runner +
	// Queue + Audit + ReadyPub) so future modules can call into the
	// reports surface without re-constructing it.
	LocatorService = "reports.Service"
	// LocatorJobQueue publishes the JobQueue port (the asynq-backed
	// Queue) so external callers can enqueue reports without re-wiring
	// the asynq client + store.
	LocatorJobQueue = "reports.JobQueue"
	// LocatorAsynqClient publishes the asynq.Client cmd/api built so
	// cmd/worker can reuse the same client for status look-ups.
	LocatorAsynqClient = "reports.AsynqClient"
	// LocatorStore publishes the *reportstore.PG so cmd/worker's
	// Consumer can share the same store instance.
	LocatorStore = "reports.Store"
	// LocatorAuditEmitter publishes the AuditEmitter the Consumer
	// reuses inside its succeed/fail tx.
	LocatorAuditEmitter = "reports.AuditEmitter"
	// LocatorReportReadyPublisher publishes the ReportReadyPublisher
	// the Consumer invokes on successful upload.
	LocatorReportReadyPublisher = "reports.ReportReadyPublisher"
	// LocatorBucket publishes the resolved S3 bucket name. cmd/worker
	// reads this rather than re-deriving from cfg.S3.Buckets.Reports
	// so a future renaming has a single source of truth.
	LocatorBucket = "reports.Bucket"
)

// Config groups the construction-time parameters of the module.
//
// ObjectStore is the storage backend the Consumer uploads to; it
// mirrors recording.Config.ObjectStore (Plan 12.2). In dev/test
// cmd/api passes storage.LocalObjectStore; production passes the
// build-tag-gated Yandex adapter.
//
// When ObjectStore is nil the module logs WARN and skips wiring the
// Queue/Consumer paths (HTTP routes still mount; sync Runner.Run is
// available — the artifact-bound async path is what needs S3).
type Config struct {
	ObjectStore storage.ObjectStore
}

// Module is the top-level registration handle for the reports module.
// Stateless aside from its Config — safe to share across goroutines.
type Module struct {
	cfg Config
}

// Compile-time check that *Module satisfies modules.Module.
var _ modules.Module = (*Module)(nil)

// New returns a fresh Module ready for Register. cmd/api passes the
// resolved ObjectStore (storage.LocalObjectStore for dev, Yandex for
// prod) so the Consumer side wired by cmd/worker has somewhere to
// upload rendered artifacts.
func New(cfg Config) *Module {
	return &Module{cfg: cfg}
}

// Name returns the module's unique identifier within the registry.
func (*Module) Name() string { return "reports" }

// Register wires the reports module. See the package doc comment for
// the degraded-boot matrix.
//
//nolint:gocognit,gocyclo // composition-root style: linear sequence of locator pulls + degraded-boot fallbacks
func (m *Module) Register(d modules.Deps) error {
	if d.Logger == nil {
		return errors.New("reports: Deps.Logger is required")
	}
	logger := d.Logger.Named("reports")

	if d.Config == nil {
		return errors.New("reports: Deps.Config is required")
	}
	if d.Pool == nil {
		// No DB → no reports subsystem. Worker-only boot path (no
		// Postgres) falls through here; cmd/api always supplies Pool.
		logger.Info("reports: Pool unavailable, module disabled")
		return nil
	}
	if d.HTTPRouter == nil {
		logger.Info("reports: HTTP router unavailable, module disabled")
		return nil
	}

	// Apply config defaults in-place (Plan 13.3 Task 8 — the
	// ReportsConfig.Validate is also wired into the top-level
	// Config.Validate, so this is a belt-and-braces safeguard for
	// callers that build a *Config directly without going through
	// pkg/config.Load).
	cfg := &d.Config.Reports
	_ = cfg.Validate()

	bucket := d.Config.S3.Buckets.Reports
	if bucket == "" {
		logger.Warn("reports: cfg.S3.Buckets.Reports empty, module disabled")
		return nil
	}

	ana, ok := lookupAnalyticsServiceRO(d.Locator, logger)
	if !ok {
		logger.Warn("reports: analytics.ServiceRO not in locator, module disabled (run analytics module first)")
		return nil
	}

	rbac, ok := lookupRBACChecker(d.Locator, logger)
	if !ok {
		logger.Warn("reports: auth.RBACChecker not in locator, module disabled (run auth module first)")
		return nil
	}

	// Outbox writer is stateless (zero-value PostgresWriter); no
	// locator round-trip needed.
	outboxWriter := outbox.NewPostgresWriter()

	store := reportstore.NewPG(d.Pool)
	audit := rptservice.NewAuditEmitter(outboxWriter)
	readyPub := rptevents.NewReportReadyPublisher(outboxWriter)

	svc := rptservice.Build(ana, d.Pool, audit, readyPub, rptservice.Config{
		Threshold: rptservice.ThresholdConfig{
			AsyncPeriodDays:   cfg.AsyncThresholdPeriodDays,
			AsyncRowThreshold: cfg.AsyncThresholdRecords,
		},
	})

	// Async path requires Redis (asynq) + a real ObjectStore. Either
	// missing → log WARN, skip Queue/Consumer wiring; the sync
	// Runner.Run continues to serve fast/small exports.
	var asynqClient *asynq.Client
	if d.Redis == nil {
		logger.Warn("reports: Redis unavailable, async Queue disabled")
	} else if m.cfg.ObjectStore == nil {
		logger.Warn("reports: ObjectStore unavailable, async Queue disabled")
	} else {
		// asynqClient owns a Redis connection pool. We do NOT call
		// .Close() — cmd/api has no module-level Stop hook today
		// (Module.Register is a one-shot wiring API). The OS releases
		// the FDs at process exit; in tests that boot cmd/api in-process
		// the leak is bounded by the process lifecycle. If a future
		// PRD adds Module.Stop, wire asynqClient.Close into it.
		asynqClient = asynq.NewClient(asynq.RedisClientOpt{
			Addr:     d.Config.Database.Redis.Addr,
			Password: d.Config.Database.Redis.Password,
			DB:       d.Config.Database.Redis.DB,
		})
		svc.Queue = rptservice.NewQueue(store, d.Pool, asynqClient, cfg.QueueName)
	}

	// HTTP transport. Routes mount under /api/reports via Register.
	handlers := rpthttp.NewHandlers(svc)
	rpthttp.Register(d.HTTPRouter, rpthttp.RouterDeps{
		Handlers:     handlers,
		Resolver:     store,
		RequireAdmin: requireAdmin(rbac),
	})

	// Publish locator entries for cmd/worker.
	if d.Locator != nil {
		d.Locator.Register(LocatorService, svc)
		d.Locator.Register(LocatorStore, store)
		d.Locator.Register(LocatorAuditEmitter, audit)
		d.Locator.Register(LocatorReportReadyPublisher, readyPub)
		d.Locator.Register(LocatorBucket, bucket)
		if svc.Queue != nil {
			d.Locator.Register(LocatorJobQueue, svc.Queue)
		}
		if asynqClient != nil {
			d.Locator.Register(LocatorAsynqClient, asynqClient)
		}
	}

	logger.Info("reports module registered",
		zap.String("bucket", bucket),
		zap.Duration("presign_ttl", cfg.PresignedURLTTL),
		zap.Int("async_period_days", cfg.AsyncThresholdPeriodDays),
		zap.Int("async_row_threshold", cfg.AsyncThresholdRecords),
		zap.String("queue", cfg.QueueName),
		zap.Bool("queue_wired", svc.Queue != nil),
	)
	return nil
}

// requireAdmin returns a gin middleware enforcing the caller has admin
// privileges on the reports module. Defence-in-depth: BOTH a role-list
// fast-path (admin role on the JWT claims) AND a fallback RBACChecker
// lookup against ActionReportGenerate / ActionReportList — mirrors the
// recording module's transport-level requireRole but adds the RBAC
// matrix check so a future "report.list-only" sub-role works without
// changing this code.
//
// On a missing-claims chain (route mounted without JWTMiddleware) we
// abort with 401 rather than 403; surfacing the wiring bug as 401
// keeps the diagnostic clean. On forbidden we abort with the canonical
// {code, message} envelope.
//
// GET routes (list-kinds + list-jobs) check ActionReportList; POST
// routes (export + custom) check ActionReportGenerate. Path-param
// routes inherit the action of the HTTP verb.
func requireAdmin(checker authapi.RBACChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, rpthttp.ErrorEnvelope{
				Code:    "reports.unauthenticated",
				Message: "missing auth claims",
			})
			return
		}
		// Fast-path: an admin or supervisor role on the claims short-
		// circuits the matrix call. Operators never have report.*
		// permissions, so the slice contains exactly the two elevated
		// roles. Resource-id ownership checks are not relevant here —
		// reports are tenant-scoped, not user-scoped.
		if slices.ContainsFunc([]authapi.Role{authapi.RoleAdmin, authapi.RoleSupervisor},
			func(r authapi.Role) bool { return claims.HasRole(r) }) {
			c.Next()
			return
		}
		// Fallback: an authenticated user without the elevated roles
		// could still be permitted by the matrix if it's been
		// re-configured. The matrix returns nil on permit, error
		// otherwise; we surface the error message in the envelope so
		// dev/test envs can introspect the deny reason.
		action := authapi.ActionReportGenerate
		if c.Request.Method == http.MethodGet {
			action = authapi.ActionReportList
		}
		if err := checker.Check(c.Request.Context(), claims, action,
			authapi.ResourceTenantWide("report")); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, rpthttp.ErrorEnvelope{
				Code:    "reports.forbidden",
				Message: err.Error(),
			})
			return
		}
		c.Next()
	}
}

// lookupAnalyticsServiceRO pulls the analytics read-side surface from
// the locator. The producing key (analyticsmod.LocatorMetricsQuery) is
// registered by internal/analytics/module.go::Register; in our
// degraded-boot matrix a missing entry means analytics never wired —
// we WARN and skip the rest of reports' boot. The producing module
// registers *service.QueryService, which satisfies analyticsapi.ServiceRO.
func lookupAnalyticsServiceRO(loc modules.ServiceLocator, logger *zap.Logger) (analyticsapi.ServiceRO, bool) {
	if loc == nil {
		return nil, false
	}
	raw, ok := loc.Lookup(analyticsmod.LocatorMetricsQuery)
	if !ok {
		return nil, false
	}
	ro, ok := raw.(analyticsapi.ServiceRO)
	if !ok {
		logger.Error("reports: analytics.MetricsQuery registered with wrong type — skipping",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil, false
	}
	return ro, true
}

// lookupRBACChecker pulls auth.RBACChecker out of the locator. Mirrors
// recording.lookupRBACChecker; returns ok=false when missing or
// type-mismatched so the caller can surface a clean warning.
func lookupRBACChecker(loc modules.ServiceLocator, logger *zap.Logger) (authapi.RBACChecker, bool) {
	if loc == nil {
		return nil, false
	}
	raw, ok := loc.Lookup(authmod.LocatorRBACChecker)
	if !ok {
		return nil, false
	}
	rb, ok := raw.(authapi.RBACChecker)
	if !ok {
		logger.Error("reports: auth.RBACChecker registered with wrong type — skipping",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil, false
	}
	return rb, true
}

// Keep redis import alive for golangci-lint's `unused` linter — the
// asynq.RedisClientOpt struct used in Register satisfies redis's
// connection interface but we don't reference the redis package
// directly. Drop this var when a future change references redis.* in
// the module body.
var _ = redis.Nil

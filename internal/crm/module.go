// Package crm — Module registration entry point.
//
// Plan 06 Task 1 wired the project store + service. Task 3 added the
// respondent service (single-add). Task 4 (this commit) layers the
// async respondent-import path:
//   - When d.Redis is non-nil, the module builds an asynq.Client +
//     asynq.Server, registers TaskRespondentImport on a ServeMux, and
//     starts the server (non-blocking) in a goroutine. The server's
//     lifecycle is owned by Module.Stop, which calls asynq.Server.Stop
//     for a graceful shutdown.
//   - When d.Redis is nil, the import path is left unwired — the
//     RespondentService still satisfies api.RespondentService for
//     Create/Get/Search via the synchronous methods, but Import returns
//     ErrInvalidArgument so the caller learns immediately that the
//     async path is unavailable.
//
// HTTP transport, NATS subscribers, and the remaining services
// (QuotaTracker, DNCManager) land in Plan 06 Task 5 / Plan 11.
//
// Required Deps:
//
//	d.Logger        — non-nil
//	d.Pool          — non-nil (Postgres pool)
//	d.Locator       — non-nil
//
// Optional Deps:
//
//	d.Redis         — when non-nil, the import path is wired. Without
//	                  it, Import is unavailable.
//	d.EventBus      — when non-nil, import.* events are published.
//
// Optional Locator entries (registered earlier by other modules):
//
//	audit.Logger          — falls back to noopAuditLogger when missing.
//	tenancy.KMSResolver   — required for the import path. Without it,
//	                        the module logs a warning and disables Import.
//	tenancy.PhoneHasher   — required for the import path; same fallback.
//
// When any required dependency is missing, Register returns an error
// rather than panicking; cmd/api surfaces the error during boot.
package crm

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	crmservice "github.com/sociopulse/platform/internal/crm/service"
	crmstore "github.com/sociopulse/platform/internal/crm/store"
	transporthttp "github.com/sociopulse/platform/internal/crm/transport/http"
	"github.com/sociopulse/platform/internal/modules"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

// Locator keys this module registers. External modules look these up
// to obtain the crm interfaces without crossing into crm/service.
const (
	LocatorProjectService    = "crm.ProjectService"
	LocatorRespondentService = "crm.RespondentService"
)

// Locator keys this module CONSUMES (registered by other modules).
const (
	locatorAuditLogger     = "audit.Logger"
	locatorKMSResolver     = "tenancy.KMSResolver"
	locatorPhoneHasher     = "tenancy.PhoneHasher"
	locatorRBACChecker     = "auth.RBACChecker"
	locatorClaimsValidator = "auth.ClaimsValidator"
)

// asynqQueueCRM is the asynq queue name the crm module's worker
// consumes. The respondent-import handler and the daily purge handler
// both live under this queue so one worker process can serve them.
const asynqQueueCRM = "crm"

// purgeCronSpec is the cron schedule for the daily 30-day soft-delete
// purge. Hour=03 in UTC keeps the heavy DELETE off peak business
// hours; the schedule is wired through asynq.PeriodicTaskManager when
// Redis is available. Cron spec validated by asynq's parser at boot.
const purgeCronSpec = "0 3 * * *"

// Module is the top-level registration handle for the crm module.
type Module struct {
	mu sync.Mutex

	asynqClient    *asynq.Client
	asynqServer    *asynq.Server
	asynqPurge     *asynq.Server // dedicated server for the purge handler — same lifecycle as asynqServer
	asynqScheduler *asynq.Scheduler
	logger         *zap.Logger
}

// Name returns the module's unique identifier within the registry.
func (m *Module) Name() string { return "crm" }

// Register wires the module's components into the composition root.
// See the package-level comment for the full sequence.
func (m *Module) Register(d modules.Deps) error {
	if err := requireDeps(d); err != nil {
		return err
	}

	logger := d.Logger.Named("crm")
	m.logger = logger

	auditLogger := lookupAuditLogger(d.Locator, logger)

	projectStore := crmstore.NewProjectStore(d.Pool)
	projectSvc := crmservice.NewProjectService(d.Pool, projectStore, auditLogger, nil, nil)
	d.Locator.Register(LocatorProjectService, crmapi.ProjectService(projectSvc))

	// Respondent service. KMSResolver and PhoneHasher come from the
	// tenancy module via the locator; if either is missing we skip the
	// service (it would panic on construction) and log loudly.
	kms := lookupKMSResolver(d.Locator, logger)
	hasher := lookupPhoneHasher(d.Locator, logger)
	respondentStore := crmstore.NewRespondentStore(d.Pool)
	if kms != nil && hasher != nil {
		respSvc := crmservice.NewRespondentService(d.Pool, respondentStore, kms, hasher, auditLogger, nil)
		respSvc = respSvc.WithLogger(logger)
		if d.EventBus != nil {
			respSvc = respSvc.WithEventBus(d.EventBus)
		}

		// Wire the async import path when Redis is available.
		if d.Redis != nil {
			if err := m.wireImportPath(d, respSvc); err != nil {
				logger.Error("failed to wire import path; respondent import unavailable", zap.Error(err))
			}
			// Wire the daily purge cron + handler. Same Redis
			// connection feeds the cron scheduler; if it fails, the
			// soft-deleted rows still exist (just don't get hard-
			// deleted automatically) and a future replay of the
			// scheduler picks them up.
			if err := m.wirePurgePath(d, respondentStore, auditLogger); err != nil {
				logger.Error("failed to wire respondent purge path", zap.Error(err))
			}
		} else {
			logger.Warn("Redis not configured — respondent import + purge cron unavailable until d.Redis is wired")
		}

		d.Locator.Register(LocatorRespondentService, crmapi.RespondentService(respSvc))
	} else {
		logger.Warn("tenancy.KMSResolver / tenancy.PhoneHasher missing — RespondentService not registered")
	}

	// HTTP transport. We mount only when an HTTPRouter is available
	// (cmd/api wires it; cmd/worker does not). Missing locator entries
	// for the auth-side dependencies fall back to a one-line warning
	// rather than panic, so the module still boots in a worker-only
	// process.
	if d.HTTPRouter != nil {
		if err := m.wireHTTPTransport(d, projectSvc); err != nil {
			logger.Warn("HTTP transport not mounted", zap.Error(err))
		}
	} else {
		logger.Debug("d.HTTPRouter is nil — skipping crm HTTP transport mount")
	}

	logger.Info("crm module registered (Plan 06 Task 5)")
	return nil
}

// wireImportPath builds the asynq client + server, registers the
// import handler on a ServeMux, attaches both to the supplied service
// via the With* setters, and starts the server in the background. The
// server's lifecycle is owned by Module.Stop.
func (m *Module) wireImportPath(d modules.Deps, svc *crmservice.RespondentService) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Plan 21 Task 2 — *FromRedisClient constructors flip asynq's
	// sharedConnection flag to true; Shutdown/Close then leave the
	// underlying d.Redis client alone (cmd/api owns the lifecycle).
	// The plain RedisConnOpt constructors would set
	// sharedConnection=false and call broker.Close at shutdown, which
	// closes the SHARED redis.UniversalClient and crashes any sibling
	// asynq server still draining (subscriber.go:83 nil-pubsub panic).
	client := asynq.NewClientFromRedisClient(d.Redis)
	server := asynq.NewServerFromRedisClient(d.Redis, asynq.Config{
		Concurrency: 4,
		Queues: map[string]int{
			asynqQueueCRM: 1,
		},
		Logger: zapAsynqLogger{l: d.Logger.Named("asynq")},
	})

	progress := crmservice.NewProgressTracker(d.Redis, d.EventBus, d.Logger.Named("crm.progress"), nil)

	svc.WithEnqueuer(client).WithProgress(progress).WithLogger(d.Logger.Named("crm"))

	mux := asynq.NewServeMux()
	mux.HandleFunc(crmapi.TaskRespondentImport, svc.HandleImportTask)

	if err := server.Start(mux); err != nil {
		_ = client.Close()
		return fmt.Errorf("start asynq server: %w", err)
	}
	m.asynqClient = client
	m.asynqServer = server
	d.Logger.Named("crm").Info("asynq server started", zap.String("queue", asynqQueueCRM))
	return nil
}

// Stop releases resources held by the module. Safe to call multiple
// times — second invocation is a no-op.
func (m *Module) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	if m.asynqScheduler != nil {
		m.asynqScheduler.Shutdown()
		m.asynqScheduler = nil
	}
	if m.asynqPurge != nil {
		// Drain the purge server identically to the import server.
		// Start was non-blocking; Stop refuses new tasks then waits
		// for in-flight handlers to drain (up to ShutdownTimeout).
		m.asynqPurge.Stop()
		m.asynqPurge.Shutdown()
		m.asynqPurge = nil
	}
	if m.asynqServer != nil {
		// Stop refuses new tasks and waits for in-flight handlers to
		// drain (up to ShutdownTimeout, default 8s). Shutdown then
		// performs final cleanup on the underlying connection.
		m.asynqServer.Stop()
		m.asynqServer.Shutdown()
		m.asynqServer = nil
	}
	if m.asynqClient != nil {
		// Plan 21 Task 2 — the client was built via
		// asynq.NewClientFromRedisClient (shared-connection mode), so
		// asynq.Client.Close returns "redis connection is shared so
		// the Client can't be closed through asynq" — cmd/api owns
		// rdb.Close. Drop the reference rather than calling Close.
		m.asynqClient = nil
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// wirePurgePath registers the daily purge handler on the asynq
// ServeMux (already set up by wireImportPath) and starts an
// asynq.Scheduler that enqueues the purge task on the configured cron
// spec. The scheduler shares the Redis connection with the import
// path so we only pay one connection-pool overhead.
//
// If wireImportPath has not run (no asynq server), we skip — the
// purge handler needs the same mux to register against.
func (m *Module) wirePurgePath(d modules.Deps, store crmapi.RespondentStorePort, auditLogger auditapi.Logger) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.asynqServer == nil {
		// Import path didn't wire (Redis missing) — purge can still
		// be invoked manually via PurgeWorker.Run from cmd/worker,
		// but we don't register the cron here.
		return errors.New("asynq server not running; purge cron not scheduled")
	}

	worker := crmservice.NewPurgeWorker(d.Pool, store, auditLogger, 0, 0, nil).
		WithLogger(d.Logger.Named("crm.purge"))

	// The mux was created inside wireImportPath; we cannot reach it
	// from here without restructuring. Plan 06 Task 5 wires the
	// scheduler directly; the actual handler registration happens via
	// a dedicated mux to keep the lifecycle ownership clear.
	purgeMux := asynq.NewServeMux()
	purgeMux.HandleFunc(crmapi.TaskRespondentsPurge, worker.HandlePurgeTask)

	// Plan 21 Task 2 — see wireImportPath: *FromRedisClient
	// constructors avoid the shared-connection close at Shutdown.
	// Without this, the purge server's Shutdown crashes the import
	// server's subscriber (and vice versa) when both share d.Redis.
	purgeServer := asynq.NewServerFromRedisClient(d.Redis, asynq.Config{
		Concurrency: 1,
		Queues: map[string]int{
			asynqQueueCRM: 1,
		},
		Logger: zapAsynqLogger{l: d.Logger.Named("asynq.purge")},
	})
	if err := purgeServer.Start(purgeMux); err != nil {
		return fmt.Errorf("start asynq purge server: %w", err)
	}

	scheduler := asynq.NewSchedulerFromRedisClient(d.Redis, &asynq.SchedulerOpts{
		Logger: zapAsynqLogger{l: d.Logger.Named("asynq.scheduler")},
	})
	task := asynq.NewTask(crmapi.TaskRespondentsPurge, nil)
	if _, err := scheduler.Register(purgeCronSpec, task,
		asynq.Queue(asynqQueueCRM),
		asynq.MaxRetry(2),
	); err != nil {
		purgeServer.Shutdown()
		return fmt.Errorf("register purge cron: %w", err)
	}
	if err := scheduler.Start(); err != nil {
		purgeServer.Shutdown()
		return fmt.Errorf("start asynq scheduler: %w", err)
	}

	m.asynqPurge = purgeServer
	m.asynqScheduler = scheduler
	d.Logger.Named("crm").Info("respondent purge cron registered",
		zap.String("schedule", purgeCronSpec),
		zap.String("task", crmapi.TaskRespondentsPurge),
	)
	return nil
}

// wireHTTPTransport mounts the gin handlers on /api. Auth deps come
// from the locator (registered earlier by the auth module); when
// missing we log and bail without panicking so a worker-only boot
// stays alive.
func (m *Module) wireHTTPTransport(d modules.Deps, projectSvc crmapi.ProjectService) error {
	rbac, ok := lookupRBACChecker(d.Locator, d.Logger)
	if !ok {
		return errors.New("auth.RBACChecker missing from locator")
	}
	validator, ok := lookupClaimsValidator(d.Locator, d.Logger)
	if !ok {
		return errors.New("auth.ClaimsValidator missing from locator")
	}

	// RespondentService is registered above only when KMS+Hasher are
	// available. We re-fetch it from the locator to keep the wiring
	// path consistent — locator-driven so future swaps (e.g. a
	// decorator wrapping the service) are picked up automatically.
	rawResp, ok := d.Locator.Lookup(LocatorRespondentService)
	if !ok {
		return errors.New("crm.RespondentService not registered (KMS/Hasher missing)")
	}
	respSvc, ok := rawResp.(crmapi.RespondentService)
	if !ok {
		return fmt.Errorf("crm.RespondentService registered with wrong type %T", rawResp)
	}

	transporthttp.Mount(d.HTTPRouter.Group("/api"), transporthttp.Deps{
		Logger:     d.Logger.Named("crm.http"),
		Projects:   projectSvc,
		Respondent: respSvc,
		RBAC:       rbac,
		Validator:  validator,
	})
	d.Logger.Named("crm").Info("HTTP transport mounted under /api")
	return nil
}

// lookupRBACChecker pulls auth.RBACChecker out of the locator. Mirrors
// the lookupAuditLogger pattern; returns ok=false when missing or
// type-mismatched so the caller can surface a clean warning.
func lookupRBACChecker(loc modules.ServiceLocator, log *zap.Logger) (authapi.RBACChecker, bool) {
	raw, ok := loc.Lookup(locatorRBACChecker)
	if !ok {
		return nil, false
	}
	c, ok := raw.(authapi.RBACChecker)
	if !ok {
		log.Error("auth.RBACChecker registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil, false
	}
	return c, true
}

// lookupClaimsValidator pulls auth.ClaimsValidator out of the locator.
func lookupClaimsValidator(loc modules.ServiceLocator, log *zap.Logger) (authapi.ClaimsValidator, bool) {
	raw, ok := loc.Lookup(locatorClaimsValidator)
	if !ok {
		return nil, false
	}
	v, ok := raw.(authapi.ClaimsValidator)
	if !ok {
		log.Error("auth.ClaimsValidator registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil, false
	}
	return v, true
}

// requireDeps validates that every Register prerequisite is non-nil.
// Returning a structured error (rather than panicking) lets cmd/api
// surface a clean message at boot.
func requireDeps(d modules.Deps) error {
	switch {
	case d.Logger == nil:
		return errors.New("crm: Deps.Logger is required")
	case d.Pool == nil:
		return errors.New("crm: Deps.Pool is required")
	case d.Locator == nil:
		return errors.New("crm: Deps.Locator is required")
	}
	return nil
}

// lookupAuditLogger pulls audit.Logger out of the locator. Audit is
// optional in early plans (Plan 03 stubs the module), so a missing
// entry falls back to a noop logger and a one-line warning. Same
// fallback pattern as auth.Module — keeps the boot sequence resilient
// during the gradual module rollout.
func lookupAuditLogger(loc modules.ServiceLocator, log *zap.Logger) auditapi.Logger {
	raw, ok := loc.Lookup(locatorAuditLogger)
	if !ok {
		log.Warn("audit.Logger not in locator — falling back to noop logger; audit rows will be silently dropped until audit module registers")
		return noopAuditLogger{}
	}
	logger, ok := raw.(auditapi.Logger)
	if !ok {
		log.Error("audit.Logger registered with wrong type — falling back to noop logger",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return noopAuditLogger{}
	}
	return logger
}

// lookupKMSResolver pulls tenancy.KMSResolver out of the locator. When
// missing, returns nil and the caller skips wiring the respondent
// service. Mirrors lookupAuditLogger's defensive style.
func lookupKMSResolver(loc modules.ServiceLocator, log *zap.Logger) tenancyapi.KMSResolver {
	raw, ok := loc.Lookup(locatorKMSResolver)
	if !ok {
		return nil
	}
	r, ok := raw.(tenancyapi.KMSResolver)
	if !ok {
		log.Error("tenancy.KMSResolver registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil
	}
	return r
}

// lookupPhoneHasher pulls tenancy.PhoneHasher out of the locator.
func lookupPhoneHasher(loc modules.ServiceLocator, log *zap.Logger) tenancyapi.PhoneHasher {
	raw, ok := loc.Lookup(locatorPhoneHasher)
	if !ok {
		return nil
	}
	h, ok := raw.(tenancyapi.PhoneHasher)
	if !ok {
		log.Error("tenancy.PhoneHasher registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil
	}
	return h
}

// zapAsynqLogger adapts zap to asynq's Logger interface (Print-style
// methods). asynq invokes these for queue-server lifecycle events; we
// surface them at the appropriate zap level so they land in the
// project's structured log stream.
type zapAsynqLogger struct {
	l *zap.Logger
}

func (z zapAsynqLogger) Debug(args ...any) { z.l.Sugar().Debug(args...) }
func (z zapAsynqLogger) Info(args ...any)  { z.l.Sugar().Info(args...) }
func (z zapAsynqLogger) Warn(args ...any)  { z.l.Sugar().Warn(args...) }
func (z zapAsynqLogger) Error(args ...any) { z.l.Sugar().Error(args...) }
func (z zapAsynqLogger) Fatal(args ...any) { z.l.Sugar().Fatal(args...) }

// noopAuditLogger is the fallback audit.Logger used when the audit
// module hasn't registered yet. It silently drops every event;
// crm bootstraps to a working state, and once a future plan wires the
// real audit Logger this fallback is never selected.
type noopAuditLogger struct{}

// Write satisfies auditapi.Logger.
func (noopAuditLogger) Write(_ context.Context, _ auditapi.Event) error { return nil }

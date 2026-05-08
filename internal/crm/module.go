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
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	crmservice "github.com/sociopulse/platform/internal/crm/service"
	crmstore "github.com/sociopulse/platform/internal/crm/store"
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
	locatorAuditLogger = "audit.Logger"
	locatorKMSResolver = "tenancy.KMSResolver"
	locatorPhoneHasher = "tenancy.PhoneHasher"
)

// asynqQueueCRM is the asynq queue name the crm module's worker
// consumes. Plan 06 Task 5+ may add more (purge, recompute) — they all
// live under the same queue so one worker process can serve them.
const asynqQueueCRM = "crm"

// Module is the top-level registration handle for the crm module.
type Module struct {
	mu sync.Mutex

	asynqClient *asynq.Client
	asynqServer *asynq.Server
	logger      *zap.Logger
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
		} else {
			logger.Warn("Redis not configured — respondent import unavailable until d.Redis is wired")
		}

		d.Locator.Register(LocatorRespondentService, crmapi.RespondentService(respSvc))
	} else {
		logger.Warn("tenancy.KMSResolver / tenancy.PhoneHasher missing — RespondentService not registered")
	}

	logger.Info("crm module registered (Plan 06 Task 4)")
	return nil
}

// wireImportPath builds the asynq client + server, registers the
// import handler on a ServeMux, attaches both to the supplied service
// via the With* setters, and starts the server in the background. The
// server's lifecycle is owned by Module.Stop.
func (m *Module) wireImportPath(d modules.Deps, svc *crmservice.RespondentService) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	redisOpt := buildAsynqRedisOpt(d.Redis)
	client := asynq.NewClient(redisOpt)
	server := asynq.NewServer(redisOpt, asynq.Config{
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
	if m.asynqServer != nil {
		// Stop refuses new tasks and waits for in-flight handlers to
		// drain (up to ShutdownTimeout, default 8s). Shutdown then
		// performs final cleanup on the underlying connection.
		m.asynqServer.Stop()
		m.asynqServer.Shutdown()
		m.asynqServer = nil
	}
	if m.asynqClient != nil {
		if err := m.asynqClient.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close asynq client: %w", err))
		}
		m.asynqClient = nil
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
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

// buildAsynqRedisOpt constructs asynq's RedisConnOpt from the project's
// existing Redis client. asynq insists on an opt that exposes a
// MakeRedisClient method; passing the universal client we already
// hold is the simplest way to keep the connection pool shared.
func buildAsynqRedisOpt(rdb redis.UniversalClient) asynq.RedisConnOpt {
	return asynqClientOpt{client: rdb}
}

// asynqClientOpt is a private adapter that satisfies asynq.RedisConnOpt
// by returning the supplied UniversalClient verbatim. This way the
// crm module reuses cmd/api's connection pool instead of opening a
// second one.
type asynqClientOpt struct {
	client redis.UniversalClient
}

// MakeRedisClient returns the wrapped client. asynq accepts an
// interface{} so we don't need to import a more specific type.
func (o asynqClientOpt) MakeRedisClient() any { return o.client }

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

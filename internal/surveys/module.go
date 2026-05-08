// Package surveys — Module registration entry point.
//
// Plan 07 Tasks 4–6 wire:
//  1. Schema validator (JSON-Schema + graph) with the production
//     DSL evaluator (expr-lang/expr backed).
//  2. Postgres SurveyStore + VersionStore.
//  3. SurveyService consuming the above plus the audit logger
//     (looked up via locator with a noop fallback when audit hasn't
//     registered yet).
//  4. Runtime (Plan 07 Task 5) for preview/run.
//  5. HTTP transport (Plan 07 Task 6) mounted under /api/surveys when
//     auth's ClaimsValidator + RBACChecker are present in the locator.
//  6. Locator registration of `surveys.SurveyService` so other
//     modules (crm/dialer/runtime) can resolve it.
//
// NATS subscribers live in a later task (Plan 11); the slot here is
// nil-tolerant so this module boots before they exist.
//
// Required Deps:
//
//	d.Logger        — non-nil
//	d.Pool          — non-nil (Postgres pool)
//	d.Locator       — non-nil
//
// Optional Deps:
//
//	d.EventBus      — when non-nil, surveys.* events are published.
//	d.HTTPRouter    — when non-nil, /api/surveys handlers are mounted.
//
// Optional Locator entries (registered earlier by other modules):
//
//	audit.Logger          — falls back to noopAuditLogger when missing.
//	auth.ClaimsValidator  — required for HTTP mount; warning-skip when missing.
//	auth.RBACChecker      — required for HTTP mount; warning-skip when missing.
package surveys

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/internal/modules"
	surveysapi "github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/dsl"
	surveysruntime "github.com/sociopulse/platform/internal/surveys/runtime"
	"github.com/sociopulse/platform/internal/surveys/schemavalidator"
	"github.com/sociopulse/platform/internal/surveys/service"
	"github.com/sociopulse/platform/internal/surveys/store"
	transporthttp "github.com/sociopulse/platform/internal/surveys/transport/http"
)

// Locator keys this module registers. External modules look these up
// to obtain the surveys interfaces without crossing into surveys/service.
const (
	LocatorSurveyService = "surveys.SurveyService"
)

// Locator keys this module CONSUMES (registered by other modules).
const (
	locatorAuditLogger     = "audit.Logger"
	locatorClaimsValidator = "auth.ClaimsValidator"
	locatorRBACChecker     = "auth.RBACChecker"
)

// dslCacheSize is the LRU cache size for the production DSL
// evaluator's compiled programs. Sized to fit ~50 expressions per
// survey × ~50 surveys per tenant — typical workloads stay well
// under the cap.
const dslCacheSize = 4096

// runtimeCacheSize controls the schema-parse LRU inside the runtime.
// 0 = project default (256). Each tenant carries ~50 active surveys;
// the project default leaves room for transient versions during
// schema-edit churn without churning the cache.
const runtimeCacheSize = 0

// Module is the top-level registration handle for the surveys module.
// Stateless; safe to construct as a zero value.
type Module struct{}

// Name returns the module's unique identifier within the registry.
func (Module) Name() string { return "surveys" }

// Register wires the module's components into the composition root.
// See the package-level comment for the full sequence.
func (Module) Register(d modules.Deps) error {
	if err := requireDeps(d); err != nil {
		return err
	}

	logger := d.Logger.Named("surveys")
	auditLogger := lookupAuditLogger(d.Locator, logger)

	// Two evaluators: one for the schema validator (one-shot at
	// SaveVersion), one for the runtime (per-call). Both share a
	// compiled-program LRU but each instance carries its own cache so
	// validator churn doesn't evict runtime entries.
	validator, err := schemavalidator.New(dsl.NewRealEvaluator(dslCacheSize))
	if err != nil {
		return fmt.Errorf("surveys: build schema validator: %w", err)
	}
	runtime := surveysruntime.New(dsl.NewRealEvaluator(dslCacheSize), runtimeCacheSize)

	surveyStore := store.NewSurveyStore(d.Pool)
	versionStore := store.NewVersionStore(d.Pool)

	svc := service.NewSurveyService(
		d.Pool,
		surveyStore,
		versionStore,
		validator,
		auditLogger,
		d.EventBus, // nil-tolerant; Plan 11 wires real publisher
		nil,        // clock: default to time.Now
	)
	d.Locator.Register(LocatorSurveyService, surveysapi.SurveyService(svc))

	// HTTP transport (Plan 07 Task 6). Mount only when the router is
	// available (cmd/api wires it; cmd/worker does not) and the
	// auth-side dependencies are in the locator. Missing entries fall
	// back to a one-line warning rather than panic so a worker-only
	// boot stays alive.
	if d.HTTPRouter != nil {
		if err := mountHTTP(d, logger, svc, runtime, validator); err != nil {
			logger.Warn("HTTP transport not mounted", zap.Error(err))
		}
	} else {
		logger.Debug("d.HTTPRouter is nil — skipping surveys HTTP transport mount")
	}

	logger.Info("surveys module registered (Plan 07 Tasks 4–6)",
		zap.Bool("event_bus_wired", d.EventBus != nil),
		zap.Bool("http_router_wired", d.HTTPRouter != nil))
	return nil
}

// mountHTTP composes the HTTP transport's deps and mounts the gin
// handlers under /api. Auth deps come from the locator (registered
// earlier by the auth module); when missing we return a clean error
// for the caller to surface as a warning.
func mountHTTP(
	d modules.Deps,
	logger *zap.Logger,
	svc surveysapi.SurveyService,
	runtime surveysapi.Runtime,
	validator *schemavalidator.SchemaValidator,
) error {
	claimsValidator, ok := lookupClaimsValidator(d.Locator, logger)
	if !ok {
		return errors.New("auth.ClaimsValidator missing from locator")
	}
	rbac, ok := lookupRBACChecker(d.Locator, logger)
	if !ok {
		return errors.New("auth.RBACChecker missing from locator")
	}
	transporthttp.Mount(d.HTTPRouter.Group("/api"), transporthttp.Deps{
		Logger:    logger.Named("http"),
		Surveys:   svc,
		Runtime:   runtime,
		Validator: validator,
		Auth:      claimsValidator,
		RBAC:      rbac,
	})
	logger.Info("HTTP transport mounted under /api")
	return nil
}

// lookupClaimsValidator pulls auth.ClaimsValidator out of the locator.
// Mirrors the lookupAuditLogger pattern; returns ok=false when missing
// or type-mismatched so the caller can surface a clean warning.
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

// lookupRBACChecker pulls auth.RBACChecker out of the locator.
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

// requireDeps validates that every Register prerequisite is non-nil.
// Returning a structured error (rather than panicking) lets cmd/api
// surface a clean message at boot.
func requireDeps(d modules.Deps) error {
	switch {
	case d.Logger == nil:
		return errors.New("surveys: Deps.Logger is required")
	case d.Pool == nil:
		return errors.New("surveys: Deps.Pool is required")
	case d.Locator == nil:
		return errors.New("surveys: Deps.Locator is required")
	}
	return nil
}

// lookupAuditLogger pulls audit.Logger out of the locator. Audit is
// optional in early plans (Plan 03 stubs the module), so a missing
// entry falls back to a noop logger and a one-line warning. Same
// fallback pattern as auth.Module / crm.Module — keeps the boot
// sequence resilient during the gradual module rollout.
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

// noopAuditLogger is the fallback audit.Logger used when the audit
// module hasn't registered yet. It silently drops every event so the
// surveys module bootstraps to a working state; once a future plan
// wires the real audit Logger this fallback is never selected.
type noopAuditLogger struct{}

// Write satisfies auditapi.Logger.
func (noopAuditLogger) Write(_ context.Context, _ auditapi.Event) error { return nil }

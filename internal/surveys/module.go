// Package surveys — Module registration entry point.
//
// Plan 07 Task 4 wires:
//  1. Schema validator (JSON-Schema + graph) with the production
//     DSL evaluator (expr-lang/expr backed).
//  2. Postgres SurveyStore + VersionStore.
//  3. SurveyService consuming the above plus the audit logger
//     (looked up via locator with a noop fallback when audit hasn't
//     registered yet).
//  4. Locator registration of `surveys.SurveyService` so other
//     modules (crm/dialer/runtime) can resolve it.
//
// HTTP transport, NATS subscribers, and the Runtime live in later
// tasks (06+); their slots here are nil-tolerant so this module can
// boot before they exist.
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
//
// Optional Locator entries (registered earlier by other modules):
//
//	audit.Logger    — falls back to noopAuditLogger when missing.
package surveys

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	"github.com/sociopulse/platform/internal/modules"
	surveysapi "github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/dsl"
	"github.com/sociopulse/platform/internal/surveys/schemavalidator"
	"github.com/sociopulse/platform/internal/surveys/service"
	"github.com/sociopulse/platform/internal/surveys/store"
)

// Locator keys this module registers. External modules look these up
// to obtain the surveys interfaces without crossing into surveys/service.
const (
	LocatorSurveyService = "surveys.SurveyService"
)

// Locator keys this module CONSUMES (registered by other modules).
const (
	locatorAuditLogger = "audit.Logger"
)

// dslCacheSize is the LRU cache size for the production DSL
// evaluator's compiled programs. Sized to fit ~50 expressions per
// survey × ~50 surveys per tenant — typical workloads stay well
// under the cap.
const dslCacheSize = 4096

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

	validator, err := schemavalidator.New(dsl.NewRealEvaluator(dslCacheSize))
	if err != nil {
		return fmt.Errorf("surveys: build schema validator: %w", err)
	}

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

	logger.Info("surveys module registered (Plan 07 Task 4)",
		zap.Bool("event_bus_wired", d.EventBus != nil))
	return nil
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

// Package crm — Module registration entry point.
//
// Plan 06 Task 1 fills this in by:
//  1. Building the project store (internal/crm/store).
//  2. Building the project service (internal/crm/service).
//  3. Registering crm.ProjectService in d.Locator under "crm.ProjectService".
//
// HTTP transport, NATS subscribers, and the remaining services
// (RespondentService, QuotaTracker, DNCManager) land in Plan 06 Tasks 2-5.
//
// Required Deps:
//
//	d.Logger        — non-nil
//	d.Pool          — non-nil (Postgres pool)
//	d.Locator       — non-nil
//
// Optional Locator entries (registered earlier by other modules):
//
//	audit.Logger — for the audit row each Create emits. When missing,
//	               this module falls back to a noop logger and warns
//	               loudly so the gap is visible in cmd/api boot logs.
//
// When any required dependency is missing, Register returns an error
// rather than panicking; cmd/api surfaces the error during boot.
package crm

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	crmservice "github.com/sociopulse/platform/internal/crm/service"
	crmstore "github.com/sociopulse/platform/internal/crm/store"
	"github.com/sociopulse/platform/internal/modules"
)

// Locator keys this module registers. External modules look these up
// to obtain the crm interfaces without crossing into crm/service.
const (
	LocatorProjectService = "crm.ProjectService"
)

// Locator keys this module CONSUMES (registered by other modules).
const (
	locatorAuditLogger = "audit.Logger"
)

// Module is the top-level registration handle for the crm module.
// Stateless; safe to construct as a zero value.
type Module struct{}

// Name returns the module's unique identifier within the registry.
func (Module) Name() string { return "crm" }

// Register wires the module's components into the composition root.
// See the package-level comment for the full sequence.
func (Module) Register(d modules.Deps) error {
	if err := requireDeps(d); err != nil {
		return err
	}

	logger := d.Logger.Named("crm")

	auditLogger := lookupAuditLogger(d.Locator, logger)

	store := crmstore.NewProjectStore(d.Pool)
	// events publisher is nil until Plan 11 wires NATS — the service
	// silently no-ops in that path; declared subjects in api/events.go
	// are forward-compatible.
	svc := crmservice.NewProjectService(d.Pool, store, auditLogger, nil, nil)

	d.Locator.Register(LocatorProjectService, crmapi.ProjectService(svc))

	logger.Info("crm module registered (Plan 06 Task 1)")
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

// noopAuditLogger is the fallback audit.Logger used when the audit
// module hasn't registered yet. It silently drops every event;
// crm bootstraps to a working state, and once a future plan wires the
// real audit Logger this fallback is never selected.
type noopAuditLogger struct{}

// Write satisfies auditapi.Logger.
func (noopAuditLogger) Write(_ context.Context, _ auditapi.Event) error { return nil }

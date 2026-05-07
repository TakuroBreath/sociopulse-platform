// Package tenancy — Module registration entry point for cmd/api.
//
// This file is a thin shim that delegates to internal/tenancy/api.Register.
// The actual seam is set by internal/tenancy/service/register.go (Plan 04
// Task 2 onwards), which keeps internal/tenancy/api/ free of any service/
// import. cmd/api blank-imports the service package to trigger the seam's
// init().
//
// Once Plan 04 Task 2 lands, api.Register is non-nil; this file builds the
// api.Deps from modules.Deps, calls the seam, and registers the resulting
// TenantService in the modules.Locator under "tenancy.TenantService".
// SettingsCache, KMSResolver, and PhoneHasher are added in later tasks.
package tenancy

import (
	"errors"
	"fmt"

	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/internal/tenancy/api"
)

// Module is the top-level registration handle for the tenancy module. The
// type is the integration point used by cmd/api's modules.Registry.
//
// Stateless; safe to construct as a zero value. The handle returned by
// api.Register is held internally so callers can Stop() at shutdown.
type Module struct {
	apiModule *api.Module
}

// Name returns the module's unique identifier within the registry.
func (*Module) Name() string { return "tenancy" }

// Register wires the tenancy module's components into the composition root.
//
// The current Deps surface (modules.Deps) carries shared infrastructure
// such as logger, pool, eventbus, and locator. We translate that into the
// tenancy-specific api.Deps and call api.Register, which the service
// package fills in via its init().
//
// If api.Register is nil (Plan 04 Task 2 not yet landed), the call is a
// no-op and returns nil so cmd/api still boots.
func (m *Module) Register(d modules.Deps) error {
	if api.Register == nil {
		// service/register.go has not been wired yet — Plan 04 Task 2.
		return nil
	}
	deps, err := buildDeps(d)
	if err != nil {
		return fmt.Errorf("tenancy: build deps: %w", err)
	}
	mod, err := api.Register(d.Ctx, deps)
	if err != nil {
		return fmt.Errorf("tenancy: register: %w", err)
	}
	m.apiModule = mod
	if d.Locator != nil {
		// Plan 04 Task 2 only wires TenantService. The aggregate Tenancy is
		// registered later when SettingsCache/KMSResolver/PhoneHasher land.
		if ts := mod.TenantService(); ts != nil {
			d.Locator.Register("tenancy.TenantService", ts)
		}
		if t := mod.Tenancy(); t != nil {
			d.Locator.Register("tenancy.Tenancy", t)
		}
	}
	return nil
}

// Stop releases resources held by the module. Safe to call multiple times.
func (m *Module) Stop() error {
	if m.apiModule == nil {
		return nil
	}
	return m.apiModule.Stop()
}

// buildDeps translates the cross-cutting modules.Deps into the
// tenancy-specific api.Deps. KMSClient and Config are not on modules.Deps
// today; service-layer wiring (Plan 04 Task 3+) constructs them from
// d.Config and Lockbox-mounted secrets.
func buildDeps(d modules.Deps) (api.Deps, error) {
	if d.Logger == nil {
		return api.Deps{}, errors.New("tenancy: logger is required")
	}
	return api.Deps{
		Logger:     d.Logger.Named("tenancy"),
		Pool:       d.Pool,
		EventBus:   d.EventBus,
		Subscriber: d.Subscriber,
		// KMS and Config: filled by service/register.go from cmd/api's
		// config block before it sets api.Register.
	}, nil
}

// Compile-time assertion that *Module satisfies the modules.Module contract.
var _ modules.Module = (*Module)(nil)

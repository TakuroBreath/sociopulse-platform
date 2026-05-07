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
// TenantService in the modules.Locator under "tenancy.TenantService". As
// of Plan 04 Task 3, the resolver lands too — registered as
// "tenancy.KMSResolver". SettingsCache and PhoneHasher are added in later
// tasks.
package tenancy

import (
	"errors"
	"fmt"

	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/store"
	"github.com/sociopulse/platform/pkg/config"
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
		// Plan 04 Task 2 wires TenantService; Task 3 wires KMSResolver;
		// Task 5 wires PhoneHasher. SettingsCache follows in later tasks;
		// the aggregate Tenancy is registered once all four have landed.
		if ts := mod.TenantService(); ts != nil {
			d.Locator.Register("tenancy.TenantService", ts)
		}
		if r := mod.KMSResolver(); r != nil {
			d.Locator.Register("tenancy.KMSResolver", r)
		}
		if h := mod.PhoneHasher(); h != nil {
			d.Locator.Register("tenancy.PhoneHasher", h)
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
// tenancy-specific api.Deps. The KMSClient is constructed here from
// d.Config.KMS — the choice between the Yandex and local providers is
// the only place in the codebase that depends on the provider value.
func buildDeps(d modules.Deps) (api.Deps, error) {
	if d.Logger == nil {
		return api.Deps{}, errors.New("tenancy: logger is required")
	}
	if d.Config == nil {
		return api.Deps{}, errors.New("tenancy: config is required")
	}
	kmsClient, err := buildKMSClient(d.Config.KMS)
	if err != nil {
		return api.Deps{}, fmt.Errorf("tenancy: build kms client: %w", err)
	}
	return api.Deps{
		Logger:     d.Logger.Named("tenancy"),
		Pool:       d.Pool,
		EventBus:   d.EventBus,
		Subscriber: d.Subscriber,
		KMS:        kmsClient,
		// Config: api.Config is module-scoped (DEKCacheTTL, etc.). It
		// stays empty here so the resolver picks its built-in defaults
		// — Task 4 maps yaml settings into api.Config when SettingsCache
		// arrives.
	}, nil
}

// buildKMSClient picks the KMSClient implementation based on
// cfg.Provider. The empty string and "local" both select the in-process
// keychain so dev/test ergonomics don't require yaml.
//
// Anywhere else in the codebase, a switch on KMS provider would be a
// smell — this is the single place the choice lives.
func buildKMSClient(cfg config.KMSConfig) (api.KMSClient, error) {
	switch cfg.Provider {
	case "", config.KMSProviderLocal:
		return store.NewLocalKMSClient(cfg.LocalKeyHex)
	case config.KMSProviderYandex:
		// Yandex SDK is gated behind the `yandex_kms` build tag. The
		// default-build stub returns a clear error pointing operators at
		// the right escape hatch (rebuild with the tag, or drop back to
		// the local provider for dev).
		return nil, errors.New(
			"yandex KMS provider requires `-tags=yandex_kms` build; " +
				"use `kms.provider: local` for the in-process dev fallback")
	default:
		return nil, fmt.Errorf("kms: unknown provider %q (want %q or %q)",
			cfg.Provider, config.KMSProviderYandex, config.KMSProviderLocal)
	}
}

// Compile-time assertion that *Module satisfies the modules.Module contract.
var _ modules.Module = (*Module)(nil)

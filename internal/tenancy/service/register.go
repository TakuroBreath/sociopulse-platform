package service

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/store"
	"github.com/sociopulse/platform/pkg/outbox"
)

// init wires the api.Register seam. cmd/api blank-imports this package so
// the side effect runs before main starts modules.Registry. The seam keeps
// internal/tenancy/api/ free of any service/ or store/ import (api/ is the
// only package other modules may import).
func init() {
	api.Register = registerModule
}

// registerModule is the concrete api.Register implementation. It builds the
// Postgres-backed store and the TenantService, returning an *api.Module that
// the caller (internal/tenancy/module.go) registers in the modules.Locator.
//
// This Plan 04 Task 2 implementation only wires the TenantService surface;
// SettingsCache, KMSResolver, and PhoneHasher land in later tasks. The
// returned Module exposes TenantService directly so the composition root
// can register "tenancy.TenantService" in the locator without needing a
// complete Tenancy aggregate.
func registerModule(ctx context.Context, deps api.Deps) (*api.Module, error) {
	_ = ctx
	if deps.Logger == nil {
		return nil, errors.New("tenancy/service: logger is required")
	}
	if deps.Pool == nil {
		return nil, errors.New("tenancy/service: pool is required")
	}

	tenantStore := store.NewPostgresStore(deps.Pool)
	pub := newPublisher(deps.EventBus, deps.Logger)
	outboxWriter := outbox.NewPostgresWriter()
	tenantSvc := NewTenantService(deps.Logger, deps.Pool, tenantStore, deps.KMS, pub, outboxWriter)

	deps.Logger.Info("tenancy module registered",
		zap.String("service", "tenancy.TenantService"),
	)

	return api.NewModule(deps, nil /* full Tenancy aggregate lands in a later task */, tenantSvc, noopCloser{}), nil
}

// noopCloser satisfies io.Closer for module shutdown when there are no
// resources to release. Plan 04 later tasks may swap in a real closer that
// stops cache invalidation subscribers.
type noopCloser struct{}

// Close is a no-op for the Plan 04 Task 2 wiring.
func (noopCloser) Close() error { return nil }

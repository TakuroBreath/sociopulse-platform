package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

// tenantLookup is the consumer-side narrowing of tenancyapi.TenantService
// the resolver consumes. Defining it here at the consumer keeps the auth
// module from depending on the full TenantService surface.
type tenantLookup interface {
	GetByOrgCode(ctx context.Context, orgCode string) (tenancyapi.Tenant, error)
}

// TenantResolverAdapter is a thin adapter that implements TenantResolver
// by delegating to a tenancy.TenantService (or any compatible
// tenantLookup). It is the canonical wiring used by the composition
// root in internal/auth/module.go.
//
// The adapter exists so the auth Authenticator can stay decoupled from
// the tenancy module's full service surface — Authenticator needs only
// "org_code -> tenant_id" lookup, and pulling in the full TenantService
// would couple admin/data-plane lifecycles unnecessarily.
type TenantResolverAdapter struct {
	tenants tenantLookup
}

// Compile-time check that the adapter satisfies the consumer-defined
// TenantResolver interface used by the Authenticator.
var _ TenantResolver = (*TenantResolverAdapter)(nil)

// NewTenantResolverAdapter constructs a TenantResolverAdapter. svc must
// be non-nil — a nil service is a composition-root bug, not a runtime
// state, and panicking surfaces it during cmd/api startup rather than
// at first login.
func NewTenantResolverAdapter(svc tenantLookup) *TenantResolverAdapter {
	if svc == nil {
		panic("auth/service: NewTenantResolverAdapter: svc is required")
	}
	return &TenantResolverAdapter{tenants: svc}
}

// ResolveByOrgCode looks the tenant up by its public org_code and
// returns its UUID. Translates tenancyapi.ErrNotFound into the auth
// module's ErrTenantNotFound so the Authenticator's
// "wrap as ErrInvalidCredentials" branch matches a stable sentinel.
func (a *TenantResolverAdapter) ResolveByOrgCode(ctx context.Context, orgCode string) (uuid.UUID, error) {
	t, err := a.tenants.GetByOrgCode(ctx, orgCode)
	if err != nil {
		if errors.Is(err, tenancyapi.ErrNotFound) {
			return uuid.Nil, ErrTenantNotFound
		}
		return uuid.Nil, fmt.Errorf("auth/service: resolve tenant by org code: %w", err)
	}
	return t.ID, nil
}

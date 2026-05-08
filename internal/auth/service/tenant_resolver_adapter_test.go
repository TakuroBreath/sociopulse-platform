package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authservice "github.com/sociopulse/platform/internal/auth/service"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

// fakeTenantLookup is a hand-rolled lookup that returns predetermined
// values keyed by org_code. Hand-rolled rather than mockery to keep the
// test self-contained.
type fakeTenantLookup struct {
	by  map[string]tenancyapi.Tenant
	err error
}

func (f *fakeTenantLookup) GetByOrgCode(_ context.Context, orgCode string) (tenancyapi.Tenant, error) {
	if f.err != nil {
		return tenancyapi.Tenant{}, f.err
	}
	t, ok := f.by[orgCode]
	if !ok {
		return tenancyapi.Tenant{}, tenancyapi.ErrNotFound
	}
	return t, nil
}

func TestTenantResolverAdapter_HappyPath_ReturnsID(t *testing.T) {
	t.Parallel()
	want := uuid.New()
	lookup := &fakeTenantLookup{by: map[string]tenancyapi.Tenant{
		"CC-MOSKVA-01": {ID: want, OrgCode: "CC-MOSKVA-01"},
	}}
	adapter := authservice.NewTenantResolverAdapter(lookup)

	got, err := adapter.ResolveByOrgCode(context.Background(), "CC-MOSKVA-01")

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestTenantResolverAdapter_NotFound_TranslatesSentinel(t *testing.T) {
	t.Parallel()
	lookup := &fakeTenantLookup{by: map[string]tenancyapi.Tenant{}}
	adapter := authservice.NewTenantResolverAdapter(lookup)

	_, err := adapter.ResolveByOrgCode(context.Background(), "missing")

	require.Error(t, err)
	assert.ErrorIs(t, err, authservice.ErrTenantNotFound)
}

func TestTenantResolverAdapter_OtherError_Propagated(t *testing.T) {
	t.Parallel()
	want := errors.New("postgres: down")
	lookup := &fakeTenantLookup{err: want}
	adapter := authservice.NewTenantResolverAdapter(lookup)

	_, err := adapter.ResolveByOrgCode(context.Background(), "x")

	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

func TestNewTenantResolverAdapter_NilSvc_Panics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() {
		authservice.NewTenantResolverAdapter(nil)
	})
}

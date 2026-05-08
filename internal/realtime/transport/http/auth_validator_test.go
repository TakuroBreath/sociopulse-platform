package http

import (
	"context"
	"net/netip"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// fakeAuthenticator is a stub authapi.Authenticator that captures the
// last token and returns canned (Claims, error). The realtime auth
// adapter only ever calls ValidateAccessToken, so the other methods
// panic — a regression that adds an unrelated method call would surface
// at runtime rather than silently passing.
type fakeAuthenticator struct {
	gotToken string
	claims   authapi.Claims
	err      error
}

func (f *fakeAuthenticator) ValidateAccessToken(_ context.Context, token string) (authapi.Claims, error) {
	f.gotToken = token
	if f.err != nil {
		return authapi.Claims{}, f.err
	}
	return f.claims, nil
}

// The remaining authapi.Authenticator methods are not exercised by the
// adapter under test; satisfy the interface with panicking stubs so a
// regression that calls them surfaces immediately.
func (f *fakeAuthenticator) Login(context.Context, authapi.LoginInput) (authapi.AuthResult, error) {
	panic("fakeAuthenticator.Login: not used")
}

func (f *fakeAuthenticator) LoginTOTP(context.Context, authapi.LoginTOTPInput) (authapi.AuthResult, error) {
	panic("fakeAuthenticator.LoginTOTP: not used")
}

func (f *fakeAuthenticator) Refresh(context.Context, string, netip.Addr) (authapi.AuthResult, error) {
	panic("fakeAuthenticator.Refresh: not used")
}

func (f *fakeAuthenticator) Logout(context.Context, string) error {
	panic("fakeAuthenticator.Logout: not used")
}

// TestAuthValidatorAdapter_ProjectsClaims verifies the adapter projects
// uuid + Role-typed authapi.Claims onto rtapi.Claims with stringly-typed
// fields.
func TestAuthValidatorAdapter_ProjectsClaims(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userID := uuid.New()
	auth := &fakeAuthenticator{
		claims: authapi.Claims{
			UserID:   userID,
			TenantID: tenantID,
			Login:    "alice",
			Roles:    []authapi.Role{authapi.RoleAdmin, authapi.RoleSupervisor},
		},
	}
	adapter := newAuthAdapter(auth)

	got, err := adapter.Validate(context.Background(), "any-token")
	require.NoError(t, err)
	assert.Equal(t, userID.String(), got.UserID)
	assert.Equal(t, tenantID.String(), got.TenantID)
	assert.Equal(t, []string{"admin", "supervisor"}, got.Roles)
	assert.Equal(t, "any-token", auth.gotToken)
}

// TestAuthValidatorAdapter_PropagatesError verifies the adapter
// propagates the underlying error verbatim so callers can errors.Is on
// authapi.ErrTokenInvalid / ErrTokenRevoked.
func TestAuthValidatorAdapter_PropagatesError(t *testing.T) {
	t.Parallel()

	auth := &fakeAuthenticator{err: authapi.ErrTokenInvalid}
	adapter := newAuthAdapter(auth)

	got, err := adapter.Validate(context.Background(), "bad")
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrTokenInvalid)
	assert.Equal(t, rtapi.Claims{}, got)
}

// TestAuthValidatorAdapter_EmptyRoles verifies the empty-roles case
// produces a non-nil empty slice rather than a nil slice — downstream
// RBAC code uses len(claims.Roles) without a nil check.
func TestAuthValidatorAdapter_EmptyRoles(t *testing.T) {
	t.Parallel()

	auth := &fakeAuthenticator{
		claims: authapi.Claims{
			UserID:   uuid.New(),
			TenantID: uuid.New(),
			Roles:    nil,
		},
	}
	adapter := newAuthAdapter(auth)

	got, err := adapter.Validate(context.Background(), "tok")
	require.NoError(t, err)
	assert.Empty(t, got.Roles)
}

// TestRolesAsStrings_StableOrder verifies the role conversion preserves
// order — RBAC matrix membership checks iterate the slice and rely on
// stable ordering for predictable test output.
func TestRolesAsStrings_StableOrder(t *testing.T) {
	t.Parallel()

	in := []authapi.Role{authapi.RoleSupervisor, authapi.RoleOperator, authapi.RoleAdmin}
	got := rolesAsStrings(in)
	assert.Equal(t, []string{"supervisor", "operator", "admin"}, got)
}

// TestNewAuthValidator_PublicConstructor exercises the public seam
// used by internal/realtime/module.go — same behaviour as
// newAuthAdapter but the production composition root only sees the
// exported surface.
func TestNewAuthValidator_PublicConstructor(t *testing.T) {
	t.Parallel()

	auth := &fakeAuthenticator{
		claims: authapi.Claims{
			UserID:   uuid.New(),
			TenantID: uuid.New(),
			Roles:    []authapi.Role{authapi.RoleAdmin},
		},
	}
	v := NewAuthValidator(auth)
	require.NotNil(t, v)

	got, err := v.Validate(context.Background(), "tok")
	require.NoError(t, err)
	assert.Equal(t, []string{"admin"}, got.Roles)
}

// TestNewAuthAdapter_NilPanics verifies the constructor enforces the
// non-nil Authenticator contract.
func TestNewAuthAdapter_NilPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		newAuthAdapter(nil)
	})
}

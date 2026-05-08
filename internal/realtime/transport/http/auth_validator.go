package http

import (
	"context"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// authAdapter bridges the auth module's Authenticator surface (which
// returns auth.Claims with uuid.UUID + Role enums) onto the realtime
// service.AuthValidator surface (which returns rtapi.Claims with
// stringly-typed fields).
//
// The realtime layer's Connection.AuthHandshake speaks AuthValidator;
// every other module's HTTP middleware speaks ClaimsValidator. The two
// surfaces overlap conceptually but disagree on the Claims projection,
// so we keep the adapter thin and explicit rather than retro-fitting
// stringly-typed fields onto auth.Claims.
//
// The adapter is goroutine-safe — it carries no mutable state, only a
// pointer to the underlying Authenticator.
type authAdapter struct {
	auth authapi.Authenticator
}

// Compile-time guarantee the adapter satisfies the realtime
// AuthValidator contract.
var _ service.AuthValidator = (*authAdapter)(nil)

// newAuthAdapter constructs an authAdapter wrapping the supplied
// Authenticator. A nil Authenticator panics — a misconfigured
// composition root should fail at boot rather than at first
// AuthHandshake.
func newAuthAdapter(auth authapi.Authenticator) *authAdapter {
	if auth == nil {
		panic("realtime/transport/http: newAuthAdapter: auth must be non-nil")
	}
	return &authAdapter{auth: auth}
}

// NewAuthValidator is the exported constructor for the auth adapter.
// The realtime composition root (internal/realtime/module.go) calls
// this to bridge the auth module's Authenticator surface onto the
// realtime layer's AuthValidator surface used by Connection.AuthHandshake.
//
// nil auth panics — same wiring discipline as newAuthAdapter.
func NewAuthValidator(auth authapi.Authenticator) service.AuthValidator {
	return newAuthAdapter(auth)
}

// Validate parses and validates accessToken via the wrapped
// Authenticator and projects the resulting Claims onto rtapi.Claims.
//
// On success returns the projected Claims. On failure returns the
// underlying error verbatim so callers can errors.Is on
// authapi.ErrTokenInvalid / ErrTokenRevoked.
func (a *authAdapter) Validate(ctx context.Context, accessToken string) (rtapi.Claims, error) {
	c, err := a.auth.ValidateAccessToken(ctx, accessToken)
	if err != nil {
		return rtapi.Claims{}, err
	}
	return rtapi.Claims{
		UserID:   c.UserID.String(),
		TenantID: c.TenantID.String(),
		Roles:    rolesAsStrings(c.Roles),
	}, nil
}

// rolesAsStrings converts a slice of authapi.Role enum values onto
// stringly-typed roles for rtapi.Claims. Order is preserved.
//
// Returns an empty (non-nil) slice on a nil/empty input — the realtime
// RBAC layer iterates the slice without a nil-guard, so a nil result
// would cause a defensive crash that this conversion can avoid cheaply.
func rolesAsStrings(roles []authapi.Role) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}

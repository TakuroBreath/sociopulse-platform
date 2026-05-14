// Package tenant is the project-wide gin middleware that closes the
// "Tenant A admin manipulates Tenant B :id" class of cross-tenant
// breaches surfaced by the 2026-05-14 audit (Plan 13.2.5 Task 1).
//
// The middleware sits AFTER pkg/middleware/auth.JWTMiddleware on every
// admin endpoint whose :id path parameter identifies a resource owned
// by a single tenant (users, projects, respondents, surveys, ...). It
// resolves the resource's owning tenant via a caller-supplied
// ResolveTenantFn (always BypassRLS by construction — the lookup is
// definitionally cross-tenant) and aborts with 404 when that tenant
// does not match the JWT's claims.TenantID. 404 — not 403 — is
// deliberate: returning 403 would let an attacker enumerate ids by
// distinguishing "exists but not yours" from "does not exist".
//
// Defence in depth: services protected by this middleware still accept
// the caller's tenant id as an explicit first parameter. If a future
// endpoint forgets the middleware, the service-side WithTenant will
// reject the row via RLS — the explicit parameter just makes the
// invariant readable in code.
package tenant

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// ErrNotFound is the sentinel resolvers return when the requested id
// does not exist in any tenant. The middleware translates this to a
// 404 with no body — indistinguishable from a "wrong tenant" mismatch
// so attackers cannot enumerate ids.
var ErrNotFound = errors.New("tenant/middleware: resource not found")

// ResolveTenantFn maps a resource id to its owning tenant id.
// Implementations MUST use BypassRLS (the lookup is cross-tenant by
// definition) and MUST return ErrNotFound when no row matches the id.
//
// Any other error is treated as an internal failure (500) so a
// transient storage hiccup does not silently downgrade the caller's
// safety guarantee.
type ResolveTenantFn func(ctx context.Context, id uuid.UUID) (uuid.UUID, error)

// Option configures RequireSameTenant. Use WithIDParam to override the
// default ":id" path-parameter name (some routes carry the id under
// ":opID" / ":version_id" / ...). Future options can extend behaviour
// without breaking call sites — the functional-options pattern keeps
// the public surface stable.
type Option func(*config)

// WithIDParam overrides the gin path parameter name the middleware
// reads. Defaults to "id" when not set.
func WithIDParam(name string) Option {
	return func(c *config) { c.idParam = name }
}

type config struct {
	idParam string
}

const defaultIDParam = "id"

// RequireSameTenant returns a gin middleware that:
//
//  1. Parses the resource id from c.Param("id") (or the name supplied
//     via WithIDParam). Malformed → 400.
//  2. Reads the caller's tenant from the claims previously installed by
//     pkg/middleware/auth.JWTMiddleware. Missing → 401.
//  3. Calls resolveFn to determine the resource's owning tenant.
//     ErrNotFound → 404. Other error → 500.
//  4. If the caller's tenant differs from the resource's tenant →
//     404 (no body). 404 over 403 prevents existence-probe attacks.
//  5. Otherwise c.Next() proceeds to the handler.
//
// RequireSameTenant panics if resolveFn is nil so a misconfigured
// composition root surfaces at boot rather than at first request.
func RequireSameTenant(resolveFn ResolveTenantFn, opts ...Option) gin.HandlerFunc {
	if resolveFn == nil {
		panic("pkg/middleware/tenant: RequireSameTenant: resolveFn is required")
	}
	cfg := config{idParam: defaultIDParam}
	for _, opt := range opts {
		opt(&cfg)
	}
	return func(c *gin.Context) {
		raw := c.Param(cfg.idParam)
		id, err := uuid.Parse(raw)
		if err != nil {
			// Malformed id is a client problem, not an auth problem —
			// surface 400 so callers fix the URL rather than chase a
			// false-positive auth alert.
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}

		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			// JWTMiddleware did not run — programmer error. Surface
			// 401 so the rest of the chain stays oblivious.
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		ownerTenant, err := resolveFn(c.Request.Context(), id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Indistinguishable from "wrong tenant" → no body.
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			// Surface as 500. We deliberately do not attach the error
			// to c.Errors here — that would funnel the original
			// message into the per-module error envelope and risk
			// leaking storage internals through a generic guard.
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

		if ownerTenant != claims.TenantID {
			// The whole point of this middleware. 404, not 403, to
			// avoid existence-probe enumeration.
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		c.Next()
	}
}

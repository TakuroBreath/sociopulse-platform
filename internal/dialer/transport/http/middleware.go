package http

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// claimsFromContext is the central read point for the per-request
// Claims attached by JWTMiddleware. When the middleware ran on a
// route, claims are always present; the (false) branch exists for
// defence-in-depth (e.g. a future refactor that mounts a handler
// outside the JWTMiddleware chain). On the missing-claims path we
// abort with the canonical 401 envelope.
//
// Returning (Claims, true) on success / aborting with 401 on miss
// keeps the call sites a single line:
//
//	claims, ok := claimsFromContext(c)
//	if !ok { return }
//
// Sharing this helper across every handler collapses the missing-
// claims branch from N handlers into one place — both for readability
// and for coverage.
func claimsFromContext(c *gin.Context) (authapi.Claims, bool) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
			Code:    "auth.token_invalid",
			Message: "authentication required",
		})
		return authapi.Claims{}, false
	}
	return claims, true
}

// requireRole returns a gin middleware that enforces the
// authenticated caller holds at least one of the supplied roles. It
// mirrors the sibling crm/transport/http.requireAnyRole shape: if the
// request reached this layer without claims (the JWTMiddleware would
// normally have aborted) we reject with 401; otherwise 403.
//
// The RBAC matrix layer is also evaluated at the service layer for any
// state-mutating operation, but a transport-level guard surfaces the
// rejection earlier with less round-trip cost. The role list lives at
// the route declaration so each operator-facing endpoint's required
// role is auditable in a single grep.
func requireRole(roles ...authapi.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
				Code:    "auth.token_invalid",
				Message: "authentication required",
			})
			return
		}
		// slices.ContainsFunc per golang-modernize Go 1.21+; replaces the
		// explicit for-range OR-membership loop. Functionally identical.
		if slices.ContainsFunc(roles, claims.HasRole) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, ErrorEnvelope{
			Code:    "auth.insufficient_role",
			Message: "role not allowed for this action",
		})
	}
}

package http

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// claimsFromContext is the central read point for the JWT-attached claims.
// On miss (route not under JWTMiddleware) we abort with 401 — defence-in-
// depth, the route registration always pairs claimsFromContext with the
// JWTMiddleware so this branch is unreachable in production.
//
// Returning (Claims, true) on success / aborting with 401 on miss keeps
// the call sites a single line:
//
//	claims, ok := claimsFromContext(c)
//	if !ok { return }
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

// requireRole returns a gin middleware enforcing the caller holds at
// least one of the supplied roles. Mirrors
// internal/dialer/transport/http.requireRole — the role list is
// declared at the route registration so each endpoint's required role
// is auditable in a single grep.
//
// On a missing-claims chain (i.e. the JWTMiddleware is not in front of
// this route) we abort with 401 rather than 403; surfacing the wiring
// bug as 401 keeps the diagnostic clean.
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
		hasRole := slices.ContainsFunc(roles, func(r authapi.Role) bool {
			return slices.Contains(claims.Roles, r)
		})
		if !hasRole {
			c.AbortWithStatusJSON(http.StatusForbidden, ErrorEnvelope{
				Code:    "auth.insufficient_role",
				Message: "role not allowed for this action",
			})
			return
		}
		c.Next()
	}
}

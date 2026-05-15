package http

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// RouterDeps groups the wiring inputs Register needs. RBAC is the auth
// matrix checker; tests pass a no-op or denying fake.
type RouterDeps struct {
	Handlers *Handlers
	RBAC     authapi.RBACChecker
}

// Register mounts the six billing routes on r:
//
//	GET   /api/finance/dashboard   (view: admin + supervisor)
//	GET   /api/finance/projects    (view)
//	GET   /api/finance/breakdown   (view)
//	GET   /api/finance/byMonth     (view)
//	GET   /api/billing/tariffs     (view)
//	PATCH /api/billing/tariffs     (admin only)
//
// The caller wires JWT middleware EARLIER in the chain; these handlers
// expect authmw.ClaimsFromContext to succeed.
//
// Panics on a nil Handlers — every legitimate caller has them. RBAC may
// be nil; in that case the role-fast-path still permits admin+supervisor
// for view actions, but PATCH falls through to 403 because there's no
// matrix to validate the admin role's permission against.
func Register(r gin.IRouter, d RouterDeps) {
	if d.Handlers == nil {
		panic("billing/transport/http: Register: Handlers must be non-nil")
	}
	view := requireRBAC(d.RBAC, authapi.ActionBillingView)
	admin := requireRBAC(d.RBAC, authapi.ActionBillingTariffUpdate)

	finance := r.Group("/api/finance")
	finance.Use(view)
	{
		finance.GET("/dashboard", d.Handlers.Dashboard)
		finance.GET("/projects", d.Handlers.Projects)
		finance.GET("/breakdown", d.Handlers.Breakdown)
		finance.GET("/byMonth", d.Handlers.ByMonth)
	}

	tariffs := r.Group("/api/billing/tariffs")
	{
		tariffs.GET("", view, d.Handlers.GetTariffs)
		tariffs.PATCH("", admin, d.Handlers.PatchTariffs)
	}
}

// requireRBAC returns a gin middleware that enforces (claims, action).
// Mirrors internal/reports/module.go::requireAdmin — defence-in-depth
// with a role-list fast-path AND a fallback RBACChecker matrix lookup.
//
// Fast-path behaviour:
//   - For ActionBillingView: admin OR supervisor short-circuits to permit
//     (both roles see the finance dashboard).
//   - For ActionBillingTariffUpdate: only admin short-circuits.
//     Supervisor falls through to the matrix, which denies — surfacing the
//     403 the same way the canonical reports module does.
//
// On missing claims aborts 401 (NOT 403) — a missing-claims chain is a
// wiring bug (the route was mounted without JWTMiddleware); surfacing it
// as 401 keeps the diagnostic clean.
func requireRBAC(checker authapi.RBACChecker, action authapi.Action) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
				Code:    "billing.unauthenticated",
				Message: "missing auth claims",
			})
			return
		}

		// Fast-path: a role-based short-circuit. For VIEW actions both
		// admin and supervisor are permitted; for tariff-update only
		// admin is. Resource-id ownership checks are not relevant here —
		// billing routes are tenant-scoped via claims.TenantID.
		if action == authapi.ActionBillingView {
			if slices.ContainsFunc([]authapi.Role{authapi.RoleAdmin, authapi.RoleSupervisor},
				func(r authapi.Role) bool { return claims.HasRole(r) }) {
				c.Next()
				return
			}
		} else if claims.HasRole(authapi.RoleAdmin) {
			c.Next()
			return
		}

		// Fallback: a non-fast-path user could still be permitted by the
		// matrix if it's been re-configured. nil checker means no matrix
		// is available — fail closed with 403.
		if checker == nil {
			c.AbortWithStatusJSON(http.StatusForbidden, ErrorEnvelope{
				Code:    "billing.forbidden",
				Message: "rbac checker unavailable",
			})
			return
		}
		if err := checker.Check(c.Request.Context(), claims, action,
			authapi.ResourceTenantWide("billing")); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, ErrorEnvelope{
				Code:    "billing.forbidden",
				Message: err.Error(),
			})
			return
		}
		c.Next()
	}
}

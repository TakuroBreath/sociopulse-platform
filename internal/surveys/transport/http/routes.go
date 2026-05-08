package http

import (
	"net/http"

	"github.com/gin-gonic/gin"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// Mount registers every /api/surveys route on the supplied gin
// RouterGroup. The caller passes the parent (e.g. the /api group);
// Mount creates the /surveys child internally so the wire shape is
// owned by this package.
//
// Auth model:
//
//	all routes require a valid JWT (JWTMiddleware on the parent group).
//	read endpoints (GET) — operator+ via requireAnyRole.
//	preview/run — operator+ (anyone authenticated may explore).
//	write endpoints (POST/PATCH) and validate — admin via requireAdminRole.
//
// Mount panics if any required Deps field is nil so a misconfigured
// composition root fails loudly during cmd/api boot rather than at
// first request.
func Mount(group *gin.RouterGroup, deps Deps) {
	mustNotBeNil(deps)
	h := &handlers{deps: deps}

	// Every surveys route requires authentication.
	authed := group.Group("/surveys")
	authed.Use(authmw.JWTMiddleware(deps.Auth))

	// Read endpoints (operator+).
	authed.GET("", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.listSurveys)
	authed.GET("/:id", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.getSurvey)
	authed.GET("/:id/versions", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.listVersions)
	authed.GET("/:id/versions/active", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.getActiveVersion)
	authed.POST("/:id/preview/run", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.previewRun)

	// Admin write endpoints.
	admin := authed.Group("")
	admin.Use(requireAdminRole())
	admin.POST("", h.createSurvey)
	admin.PATCH("/:id", h.updateSurvey)
	admin.POST("/:id/archive", h.archiveSurvey)
	admin.POST("/:id/versions", h.saveVersion)
	admin.POST("/:id/versions/:version_id/activate", h.activateVersion)
	admin.POST("/:id/validate", h.validateSchema)
}

// mustNotBeNil verifies every required collaborator. We panic so a
// composition-root misconfiguration fails loudly during cmd/api boot.
func mustNotBeNil(d Deps) {
	switch {
	case d.Surveys == nil:
		panic("surveys/transport/http: Surveys is required")
	case d.Runtime == nil:
		panic("surveys/transport/http: Runtime is required")
	case d.Validator == nil:
		panic("surveys/transport/http: Validator is required")
	case d.Auth == nil:
		panic("surveys/transport/http: Auth is required")
	case d.RBAC == nil:
		panic("surveys/transport/http: RBAC is required")
	}
}

// requireAnyRole returns a gin middleware that enforces the
// authenticated user holds at least one of the supplied roles.
// Mirrors crm/transport/http.requireAnyRole exactly so the wire
// shape (and the rejection envelope) stays uniform across modules.
func requireAnyRole(roles ...authapi.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
				Error: "auth.token_invalid", Message: "authentication required",
			})
			return
		}
		for _, r := range roles {
			if claims.HasRole(r) {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, ErrorEnvelope{
			Error: "auth.insufficient_role", Message: "role not allowed for this action",
		})
	}
}

// requireAdminRole is the admin-only gate. Service-level RBAC is also
// checked at the matrix layer, but a transport-level guard surfaces
// the rejection earlier with less round-trip cost.
func requireAdminRole() gin.HandlerFunc {
	return requireAnyRole(authapi.RoleAdmin)
}

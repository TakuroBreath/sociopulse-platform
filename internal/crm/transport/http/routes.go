package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// Deps captures the collaborators that handlers need. Logger may be
// nil in tests — render paths gate on nil.
type Deps struct {
	Logger     *zap.Logger
	Projects   crmapi.ProjectService
	Respondent crmapi.RespondentService
	RBAC       authapi.RBACChecker
	Validator  authapi.ClaimsValidator
}

// Mount registers every /api/<crm> route on the supplied gin
// RouterGroup. The caller passes the parent (e.g. the /api group);
// Mount creates the per-resource child groups internally so the
// wire shape is owned by this package.
//
// Auth model:
//
//	all routes require a valid JWT (JWTMiddleware on the parent group).
//	read endpoints (GET) — operator+ via requireAnyRole.
//	write endpoints (POST/PATCH/DELETE) — admin via requireAdminRole.
//	GetWithPhone — admin (mirrors the canonical PII-reveal gate).
//
// Mount panics if any required Deps field is nil so a misconfigured
// composition root fails loudly during cmd/api boot rather than at
// first request.
func Mount(group *gin.RouterGroup, deps Deps) {
	mustNotBeNil(deps)
	h := &handlers{deps: deps}

	// Every crm route requires authentication.
	authed := group.Group("")
	authed.Use(authmw.JWTMiddleware(deps.Validator))

	// Projects — read endpoints (operator+).
	projects := authed.Group("/projects")
	projects.GET("", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.listProjects)
	projects.GET("/:id", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.getProject)
	projects.GET("/:id/progress", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.getProjectProgress)
	projects.GET("/:id/members", requireAnyRole(authapi.RoleSupervisor, authapi.RoleAdmin), h.listProjectMembers)

	// Projects — admin write endpoints.
	adminProjects := authed.Group("/projects")
	adminProjects.Use(requireAdminRole())
	adminProjects.POST("", h.createProject)
	adminProjects.PATCH("/:id", h.updateProject)
	adminProjects.POST("/:id/pause", h.pauseProject)
	adminProjects.POST("/:id/resume", h.resumeProject)
	adminProjects.POST("/:id/archive", h.archiveProject)
	adminProjects.POST("/:id/assign", h.assignOperators)
	adminProjects.DELETE("/:id/operators/:opID", h.unassignOperator)

	// Respondents within a project — admin creates / imports, all roles search.
	adminProjects.POST("/:id/respondents", h.createRespondent)
	adminProjects.POST("/:id/respondents/import", h.importRespondents)
	projects.GET("/:id/respondents", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.searchRespondents)

	// Respondents — by id.
	respondents := authed.Group("/respondents")
	respondents.GET("/:id", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.getRespondent)
	respondents.GET("/:id/with-phone", requireAdminRole(), h.getRespondentWithPhone)
	respondents.DELETE("/:id", requireAdminRole(), h.deleteRespondent)

	// Imports — admin.
	imports := authed.Group("/imports")
	imports.Use(requireAdminRole())
	imports.GET("/:job_id", h.getImportStatus)
}

// mustNotBeNil verifies every required collaborator. We panic so a
// composition-root misconfiguration fails loudly during cmd/api boot.
func mustNotBeNil(d Deps) {
	switch {
	case d.Projects == nil:
		panic("crm/transport/http: Projects is required")
	case d.Respondent == nil:
		panic("crm/transport/http: Respondent is required")
	case d.RBAC == nil:
		panic("crm/transport/http: RBAC is required")
	case d.Validator == nil:
		panic("crm/transport/http: Validator is required")
	}
}

// requireAnyRole returns a gin middleware that enforces the
// authenticated user holds at least one of the supplied roles.
// Mirrors auth/transport/http.requireRole but accepts an OR-set so
// read endpoints can grant access to operator OR supervisor OR admin
// without three separate route chains.
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

// requireAdminRole is the admin-only gate. The RBAC matrix layer is
// also checked at the service level (ProjectService.Create returns
// auth-layer ErrInsufficientRole when the user has no admin role),
// but a transport-level guard surfaces the rejection earlier with
// less round-trip cost.
func requireAdminRole() gin.HandlerFunc {
	return requireAnyRole(authapi.RoleAdmin)
}

package http

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
	tenantmw "github.com/sociopulse/platform/pkg/middleware/tenant"
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

	// Plan 13.2.5 Task 1 — cross-tenant guards.
	// projectSameTenant verifies the caller's tenant owns the
	// :id-from-URL project before the handler runs (404 on mismatch).
	// Same for respondents on /respondents/:id.
	projectSameTenant := tenantmw.RequireSameTenant(projectTenantResolver(deps.Projects))
	respondentSameTenant := tenantmw.RequireSameTenant(respondentTenantResolver(deps.Respondent))

	// Projects — read endpoints (operator+). All :id reads chain the
	// cross-tenant guard so an operator from Tenant A cannot probe
	// for Tenant B project ids (the BypassRLS Get would otherwise
	// return the row).
	projects := authed.Group("/projects")
	projects.GET("", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), h.listProjects)
	projects.GET("/:id", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), projectSameTenant, h.getProject)
	projects.GET("/:id/progress", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), projectSameTenant, h.getProjectProgress)
	projects.GET("/:id/members", requireAnyRole(authapi.RoleSupervisor, authapi.RoleAdmin), projectSameTenant, h.listProjectMembers)

	// Projects — admin write endpoints. Every :id mutation chains the
	// cross-tenant guard.
	adminProjects := authed.Group("/projects")
	adminProjects.Use(requireAdminRole())
	adminProjects.POST("", h.createProject)
	adminProjects.PATCH("/:id", projectSameTenant, h.updateProject)
	adminProjects.POST("/:id/pause", projectSameTenant, h.pauseProject)
	adminProjects.POST("/:id/resume", projectSameTenant, h.resumeProject)
	adminProjects.POST("/:id/archive", projectSameTenant, h.archiveProject)
	adminProjects.POST("/:id/assign", projectSameTenant, h.assignOperators)
	adminProjects.DELETE("/:id/operators/:opID", projectSameTenant, h.unassignOperator)

	// Respondents within a project — admin creates / imports, all roles search.
	// The :id is a project id; same cross-tenant guard applies.
	adminProjects.POST("/:id/respondents", projectSameTenant, h.createRespondent)
	adminProjects.POST("/:id/respondents/import", projectSameTenant, h.importRespondents)
	projects.GET("/:id/respondents", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), projectSameTenant, h.searchRespondents)

	// Respondents — by respondent id.
	respondents := authed.Group("/respondents")
	respondents.GET("/:id", requireAnyRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin), respondentSameTenant, h.getRespondent)
	respondents.GET("/:id/with-phone", requireAdminRole(), respondentSameTenant, h.getRespondentWithPhone)
	respondents.DELETE("/:id", requireAdminRole(), respondentSameTenant, h.deleteRespondent)

	// Imports — admin. job_id is an opaque async-job ticket (not a
	// row id); status lookup is tenant-scoped inside the service.
	imports := authed.Group("/imports")
	imports.Use(requireAdminRole())
	imports.GET("/:job_id", h.getImportStatus)
}

// projectTenantResolver wraps ProjectService.ResolveTenant for the
// tenant.RequireSameTenant middleware. Translates the module sentinel
// into the middleware's ErrNotFound so the response is a clean 404
// with no body.
func projectTenantResolver(p crmapi.ProjectService) tenantmw.ResolveTenantFn {
	return func(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
		t, err := p.ResolveTenant(ctx, id)
		if err != nil {
			if errors.Is(err, crmapi.ErrProjectNotFound) {
				return uuid.Nil, tenantmw.ErrNotFound
			}
			return uuid.Nil, err
		}
		return t, nil
	}
}

// respondentTenantResolver wraps RespondentService.ResolveTenant for
// the tenant.RequireSameTenant middleware.
func respondentTenantResolver(r crmapi.RespondentService) tenantmw.ResolveTenantFn {
	return func(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
		t, err := r.ResolveTenant(ctx, id)
		if err != nil {
			if errors.Is(err, crmapi.ErrRespondentNotFound) {
				return uuid.Nil, tenantmw.ErrNotFound
			}
			return uuid.Nil, err
		}
		return t, nil
	}
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

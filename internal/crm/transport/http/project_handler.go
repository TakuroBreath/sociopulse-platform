package http

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	crmservice "github.com/sociopulse/platform/internal/crm/service"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// listProjectsDefaultLimit is the default page size when ?limit is
// absent. The service layer also clamps; we surface a matching
// default so the wire shape is predictable.
const listProjectsDefaultLimit = 50

// handlers groups the per-endpoint methods so they share Deps.
type handlers struct {
	deps Deps
}

// createProject handles POST /api/projects (admin).
func (h *handlers) createProject(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	var req CreateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	in := crmapi.CreateProjectInput{
		TenantID:    claims.TenantID,
		Code:        req.Code,
		Name:        req.Name,
		Customer:    req.Customer,
		TargetCount: req.TargetCount,
		PeriodFrom:  req.PeriodFrom,
		PeriodTo:    req.PeriodTo,
		SurveyID:    req.SurveyID,
	}
	ctx := crmservice.WithActorID(c.Request.Context(), claims.UserID)
	p, err := h.deps.Projects.Create(ctx, in)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusCreated, projectToDTO(*p))
}

// listProjects handles GET /api/projects (operator+).
func (h *handlers) listProjects(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	limit := parseQueryInt(c.Query("limit"), listProjectsDefaultLimit)
	offset := parseQueryInt(c.Query("offset"), 0)
	includeArchived := strings.EqualFold(c.Query("include_archived"), "true")

	var statusPtr *crmapi.ProjectStatus
	if s := c.Query("status"); s != "" {
		ps := crmapi.ProjectStatus(s)
		statusPtr = &ps
	}

	res, err := h.deps.Projects.List(c.Request.Context(), crmapi.ListProjectsFilter{
		TenantID:        claims.TenantID,
		Search:          c.Query("search"),
		Status:          statusPtr,
		IncludeArchived: includeArchived,
		Limit:           limit,
		Offset:          offset,
	})
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, ListProjectsResponse{
		Projects:   projectsToDTO(res.Items),
		TotalCount: res.TotalCount,
	})
}

// getProject handles GET /api/projects/:id (operator+).
func (h *handlers) getProject(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	p, err := h.deps.Projects.Get(c.Request.Context(), id)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, projectToDTO(*p))
}

// updateProject handles PATCH /api/projects/:id (admin).
func (h *handlers) updateProject(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	id, perr := uuid.Parse(c.Param("id"))
	if perr != nil {
		renderBindError(c, perr)
		return
	}
	var req UpdateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	ctx := crmservice.WithActorID(c.Request.Context(), claims.UserID)
	p, err := h.deps.Projects.Update(ctx, id, crmapi.UpdateProjectInput{
		Name:        req.Name,
		Customer:    req.Customer,
		TargetCount: req.TargetCount,
		PeriodFrom:  req.PeriodFrom,
		PeriodTo:    req.PeriodTo,
		SurveyID:    req.SurveyID,
	})
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, projectToDTO(*p))
}

// pauseProject handles POST /api/projects/:id/pause (admin).
func (h *handlers) pauseProject(c *gin.Context) {
	h.transitionProject(c, h.deps.Projects.Pause)
}

// resumeProject handles POST /api/projects/:id/resume (admin).
func (h *handlers) resumeProject(c *gin.Context) {
	h.transitionProject(c, h.deps.Projects.Resume)
}

// archiveProject handles POST /api/projects/:id/archive (admin).
func (h *handlers) archiveProject(c *gin.Context) {
	h.transitionProject(c, h.deps.Projects.Archive)
}

// projectStateFn is the shared signature of ProjectService.Pause,
// Resume, and Archive — every state transition takes (ctx, id) and
// returns error.
type projectStateFn func(ctx context.Context, id uuid.UUID) error

// transitionProject is the shared body for Pause/Resume/Archive.
// Surfaces 204 on success / mapped error otherwise. The call site
// passes the bound service method; this helper handles claims +
// id parsing + actor-context attachment.
func (h *handlers) transitionProject(c *gin.Context, fn projectStateFn) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	id, perr := uuid.Parse(c.Param("id"))
	if perr != nil {
		renderBindError(c, perr)
		return
	}
	ctx := crmservice.WithActorID(c.Request.Context(), claims.UserID)
	if err := fn(ctx, id); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// getProjectProgress handles GET /api/projects/:id/progress (operator+).
func (h *handlers) getProjectProgress(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	p, perr := h.deps.Projects.GetProgress(c.Request.Context(), id)
	if perr != nil {
		renderError(c, h.deps.Logger, perr)
		return
	}
	c.JSON(http.StatusOK, progressToDTO(*p))
}

// assignOperators handles POST /api/projects/:id/assign (admin).
func (h *handlers) assignOperators(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	id, perr := uuid.Parse(c.Param("id"))
	if perr != nil {
		renderBindError(c, perr)
		return
	}
	var req AssignOperatorsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	ctx := crmservice.WithActorID(c.Request.Context(), claims.UserID)
	if err := h.deps.Projects.Assign(ctx, id, req.OperatorIDs); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// unassignOperator handles DELETE /api/projects/:id/operators/:opID (admin).
func (h *handlers) unassignOperator(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	id, perr := uuid.Parse(c.Param("id"))
	if perr != nil {
		renderBindError(c, perr)
		return
	}
	opID, oerr := uuid.Parse(c.Param("opID"))
	if oerr != nil {
		renderBindError(c, oerr)
		return
	}
	ctx := crmservice.WithActorID(c.Request.Context(), claims.UserID)
	if err := h.deps.Projects.Unassign(ctx, id, opID); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// listProjectMembers handles GET /api/projects/:id/members (supervisor+).
func (h *handlers) listProjectMembers(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	members, perr := h.deps.Projects.ListMembers(c.Request.Context(), id)
	if perr != nil {
		renderError(c, h.deps.Logger, perr)
		return
	}
	c.JSON(http.StatusOK, ListMembersResponse{
		Members: membersToDTO(members),
	})
}

// parseQueryInt parses a query-string int with a default fallback.
// Negative or unparseable values fall back to def — the service layer
// clamps the final values within its own bounds.
func parseQueryInt(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < 0 {
		return def
	}
	return int(n)
}

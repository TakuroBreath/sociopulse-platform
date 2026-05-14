package http

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// TenantResolver is the BypassRLS lookup surface the jobIDTenantGuard
// middleware needs. *reports/store.PG satisfies this via
// SelectTenantByJobID — defined consumer-side so the transport can be
// tested without dragging the pgxpool import in.
//
// The signature differs from pkg/middleware/tenant.ResolveTenantFn
// (which takes uuid.UUID): reports_jobs.id is an asynq task id (text),
// not a UUID, so the resolver works on string instead.
type TenantResolver interface {
	SelectTenantByJobID(ctx context.Context, jobID string) (uuid.UUID, error)
}

// RouterDeps groups the wiring inputs Register needs.
//
// RequireAdmin is the admin-gate middleware injected by cmd/api Task 8.
// It enforces both role membership (admin) and the RBAC matrix check
// against authapi.ActionReportGenerate / ActionReportList. Keeping the
// concrete middleware injection here lets the transport stay decoupled
// from the auth module's RBACChecker shape — production wires the
// strict checker, tests wire a no-op pass-through.
type RouterDeps struct {
	Handlers     *Handlers
	Resolver     TenantResolver
	RequireAdmin gin.HandlerFunc
}

// Register wires the reports HTTP routes onto the gin engine.
//
// Routes (all under /api/reports):
//
//	GET  /                       → ListKinds
//	POST /:kind/export           → Export
//	POST /custom                 → Custom
//	GET  /jobs/:jobID            → GetJob       (+ jobIDTenantGuard)
//	GET  /jobs/:jobID/download   → Download     (+ jobIDTenantGuard)
//
// Every route requires admin via RouterDeps.RequireAdmin. The per-jobID
// routes additionally apply jobIDTenantGuard which resolves the job's
// owning tenant via BypassRLS and aborts with 404 reports.job_not_found
// when it does not match the caller's claims — existence-probe defence,
// NOT 403.
func Register(r gin.IRouter, d RouterDeps) {
	g := r.Group("/api/reports")
	g.Use(d.RequireAdmin)

	g.GET("", d.Handlers.ListKinds)
	g.POST("/:kind/export", d.Handlers.Export)
	g.POST("/custom", d.Handlers.Custom)

	jobs := g.Group("/jobs/:jobID")
	jobs.Use(jobIDTenantGuard(d.Resolver))
	jobs.GET("", d.Handlers.GetJob)
	jobs.GET("/download", d.Handlers.Download)
}

// jobIDTenantGuard resolves the path :jobID (string — not UUID) to its
// owning tenant via the BypassRLS resolver, then compares with the
// caller's tenant from auth claims. Mirrors
// pkg/middleware/tenant.RequireSameTenant semantics but adapted for the
// text-PK shape of reports_jobs (the asynq task id is not a UUID).
//
// Behaviour:
//
//   - missing claims                              → 401 reports.unauthenticated
//   - resolver error (incl. ErrJobNotFound, DB)   → 404 reports.job_not_found
//   - tenant mismatch                             → 404 reports.job_not_found
//   - tenant match                                → c.Next()
//
// 404 (not 403) for both "missing" and "mismatch" is the
// existence-probe defence — never let an attacker distinguish
// "exists but not yours" from "does not exist".
func jobIDTenantGuard(resolver TenantResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
				Code:    "reports.unauthenticated",
				Message: "missing auth claims",
			})
			return
		}
		jobID := c.Param("jobID")
		owner, err := resolver.SelectTenantByJobID(c.Request.Context(), jobID)
		if err != nil {
			// ErrJobNotFound OR DB error — 404 either way for the
			// existence-probe defence.
			c.AbortWithStatusJSON(http.StatusNotFound, ErrorEnvelope{
				Code:    "reports.job_not_found",
				Message: "job not found",
			})
			return
		}
		if owner != claims.TenantID {
			c.AbortWithStatusJSON(http.StatusNotFound, ErrorEnvelope{
				Code:    "reports.job_not_found",
				Message: "job not found",
			})
			return
		}
		c.Next()
	}
}

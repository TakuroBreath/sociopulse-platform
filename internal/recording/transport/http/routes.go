// Package http exposes internal/recording over HTTP. The transport mounts
// three endpoints under /api:
//
//	GET  /api/calls/:call_id/recording          — admin/supervisor; streams audio.
//	GET  /api/recordings/search                 — admin/supervisor; cursor-paginated.
//	POST /api/calls/:call_id/recording/verify   — admin only; manual sha256 verify.
//
// All endpoints require an authenticated JWT (via pkg/middleware/auth.JWTMiddleware)
// and enforce role-based access via the requireRole middleware. Tenant
// isolation is enforced at the service layer via claims.TenantID.
package http

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	rapi "github.com/sociopulse/platform/internal/recording/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// RoutePrefix is the canonical mount point for recording endpoints.
const RoutePrefix = "/api"

// Deps captures the recording HTTP transport's collaborators. Validator
// may be nil — useful for tests that mock auth at a higher level (no JWT
// middleware mounted; tests pre-attach claims via the gin context).
// RBAC is reserved for future fine-grained checks; today's transport
// uses transport-level requireRole only.
type Deps struct {
	Service rapi.RecordingService

	Validator authapi.ClaimsValidator
	RBAC      authapi.RBACChecker
	Logger    *zap.Logger
}

// Mount attaches recording HTTP routes to group. group is the API root
// (typically /api), so the final paths are:
//
//	GET  /api/calls/:call_id/recording
//	GET  /api/recordings/search
//	POST /api/calls/:call_id/recording/verify
//
// All three require an authenticated JWT and either admin or supervisor
// role; verify additionally requires admin (an audit-grade action).
//
// Mount is a no-op if Deps.Service is nil — guards against a registration
// path where the recording module didn't fully wire (e.g. Postgres unreachable
// at boot, see internal/recording/module.go::Register).
func Mount(group *gin.RouterGroup, d Deps) {
	if d.Service == nil {
		return
	}

	authed := group.Group("")
	if d.Validator != nil {
		authed.Use(authmw.JWTMiddleware(d.Validator))
	}

	rb := newHandlers(d)

	// admin / supervisor reads
	authed.GET("/calls/:call_id/recording",
		requireRole(authapi.RoleAdmin, authapi.RoleSupervisor),
		rb.streamRecording)
	authed.GET("/recordings/search",
		requireRole(authapi.RoleAdmin, authapi.RoleSupervisor),
		rb.searchRecordings)

	// admin-only on verify (writes audit, may incur cost via ObjectStore.Get)
	authed.POST("/calls/:call_id/recording/verify",
		requireRole(authapi.RoleAdmin),
		rb.verifyChecksum)
}

// handlers groups the three handlers so they share the same Deps closure.
type handlers struct {
	d Deps
}

func newHandlers(d Deps) *handlers { return &handlers{d: d} }

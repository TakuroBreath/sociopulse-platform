// http_helpers.go — gin context extraction + canonical error envelope for
// the analytics HTTP transport.
//
// Tenant-from-context discipline (Plan 13.2 Task 5 § Step 5.1):
//
// The project's canonical helper is pkg/middleware/auth.ClaimsFromContext —
// it reads the decoded JWT Claims that JWTMiddleware attached under
// ClaimsContextKey. Existing transports (recording, dialer, crm) all
// resolve tenant_id via `claims, ok := authmw.ClaimsFromContext(c); …
// claims.TenantID`. We mirror that convention here — NOT the illustrative
// `c.Header.Get("X-Tenant-ID")` from the plan template.
//
// Error envelope shape: the project uses { "code": "<stable-string>",
// "message": "<human-readable>" } per internal/recording/transport/http.ErrorEnvelope
// (the canonical project transport envelope). We mirror that shape so
// dashboards / log aggregators can pivot on `code` regardless of which
// transport issued the response.
//
// Note: pkg/middleware/auth uses a DIFFERENT `{ "error": "..." }`
// envelope for its own 401 / 403 paths — this analytics transport
// deliberately does NOT mirror that shape; the canonical envelope is
// the recording/transport/http one.

package service

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// errorEnvelope is the project-canonical JSON error response shape. The
// `code` field is a stable enum string (e.g. "analytics.invalid_query")
// suitable for log-aggregation pivots; the `message` field is a
// human-readable explanation that may include user-supplied input
// (e.g. validation errors).
//
// The shape matches pkg/middleware/auth.errorEnvelope and
// internal/recording/transport/http.ErrorEnvelope. Tests assert on
// the HTTP status code, not the envelope shape, so swapping shapes
// later only requires updating this struct + respondError.
type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// tenantIDFromContext reads the tenant UUID from the JWT-attached claims.
// Returns (uuid.Nil, false) when the request did not pass through
// JWTMiddleware (defence-in-depth — the route registration in cmd/api
// must put authmw.JWTMiddleware in front of MountAnalyticsRoutes so this
// branch is unreachable in production).
//
// This helper exists as a local adapter rather than calling
// authmw.ClaimsFromContext at every handler so the analytics package
// keeps its dependency on internal/auth contained to one file. If a
// future plan adds a typed cross-module helper (e.g.
// `auth.TenantFromContext`), migrate this function at that time.
func tenantIDFromContext(c *gin.Context) (uuid.UUID, bool) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		return uuid.Nil, false
	}
	if claims.TenantID == uuid.Nil {
		return uuid.Nil, false
	}
	return claims.TenantID, true
}

// respondError writes a project-canonical errorEnvelope JSON response
// with the supplied HTTP status. Stable codes used by the analytics
// transport:
//
//	"analytics.unauthorized"   — 401, claims missing or tenant_id absent
//	"analytics.invalid_query"  — 400, gin.ShouldBindQuery validation failure
//	"analytics.invalid_window" — 400, Window.From >= To or > 1 year span
//	"analytics.internal_error" — 500, unwrapped CH / cache / crm failure
//
// 5xx bodies are intentionally generic ("query failed"); the underlying
// error stays in the structured log via handleQueryError so ops can
// triage without leaking internal details to API consumers.
func respondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, errorEnvelope{Code: code, Message: message})
}

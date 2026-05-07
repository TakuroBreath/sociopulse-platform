// Package auth is the project-wide gin middleware for HTTP authentication.
// It is the documented exception to the rule "no business types in pkg/":
// pkg/middleware/* may consume internal/<module>/api/ interfaces and
// thread the resulting domain values through *gin.Context as opaque
// payloads (per docs/architecture/01-package-layout.md § pkg/).
//
// The middleware here:
//   - extracts the Bearer token from Authorization headers,
//   - calls authapi.ClaimsValidator.Validate to authenticate it,
//   - stores the decoded Claims on the *gin.Context under a stable
//     key for downstream handlers,
//   - on failure renders the standard 401 envelope via pkg/httputil.
//
// Concrete wiring (header parsing, audience validation, error
// mapping) lands in Plan 04 Task 4.
package auth

import (
	"github.com/gin-gonic/gin"

	authapi "github.com/sociopulse/platform/internal/auth/api"
)

// ClaimsContextKey is the *gin.Context key under which JWTMiddleware
// stores the decoded Claims. Handlers retrieve the value via
// ClaimsFromContext rather than reading c.Keys directly.
const ClaimsContextKey = "sociopulse.auth.claims"

// JWTMiddleware returns a gin middleware that validates the Bearer
// token on the Authorization header against validator and stores the
// resulting Claims on the *gin.Context.
//
// On a missing or invalid token the middleware aborts the chain with
// 401 + auth.token_invalid; downstream handlers therefore never see
// an unauthenticated request.
func JWTMiddleware(validator authapi.ClaimsValidator) gin.HandlerFunc {
	panic("not implemented: see Plan 04 Task 4")
}

// ClaimsFromContext returns the Claims previously attached by
// JWTMiddleware, or zero-value Claims with ok=false if the request
// was not authenticated.
func ClaimsFromContext(c *gin.Context) (claims authapi.Claims, ok bool) {
	panic("not implemented: see Plan 04 Task 4")
}

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
//   - on failure aborts with 401 + a stable JSON error envelope.
package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	authapi "github.com/sociopulse/platform/internal/auth/api"
)

// ClaimsContextKey is the *gin.Context key under which JWTMiddleware
// stores the decoded Claims. Handlers retrieve the value via
// ClaimsFromContext rather than reading c.Keys directly.
const ClaimsContextKey = "sociopulse.auth.claims"

// bearerScheme is the only Authorization scheme the middleware accepts.
// Comparison is case-insensitive (per RFC 7235 §2.1) but the canonical
// form is `Bearer`.
const bearerScheme = "Bearer"

// errorEnvelope is the JSON shape every 401 response uses. Lives here
// rather than importing pkg/httputil to keep this middleware free of
// circular deps and decoupled from the (still-stub) ErrorHandler.
type errorEnvelope struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// JWTMiddleware returns a gin middleware that validates the Bearer
// token on the Authorization header against validator and stores the
// resulting Claims on the *gin.Context.
//
// On a missing or invalid token the middleware aborts the chain with
// 401 + auth.token_invalid; downstream handlers therefore never see
// an unauthenticated request.
//
// validator must be non-nil. A nil validator panics at construction
// time so a misconfigured composition root surfaces immediately rather
// than at first request.
func JWTMiddleware(validator authapi.ClaimsValidator) gin.HandlerFunc {
	if validator == nil {
		panic("pkg/middleware/auth: JWTMiddleware: validator is required")
	}
	return func(c *gin.Context) {
		token, ok := extractBearer(c.GetHeader("Authorization"))
		if !ok {
			abortUnauthorized(c, "auth.token_invalid", "missing or malformed Authorization header")
			return
		}
		claims, err := validator.Validate(c.Request.Context(), token)
		if err != nil {
			switch {
			case errors.Is(err, authapi.ErrTokenRevoked):
				abortUnauthorized(c, "auth.token_revoked", "token has been revoked")
			default:
				// ErrTokenInvalid and any other failure surface as
				// "invalid" — we deliberately do not leak storage or
				// signing-secret errors to the caller.
				abortUnauthorized(c, "auth.token_invalid", "token is invalid or expired")
			}
			return
		}
		c.Set(ClaimsContextKey, claims)
		c.Next()
	}
}

// ClaimsFromContext returns the Claims previously attached by
// JWTMiddleware, or zero-value Claims with ok=false if the request
// was not authenticated.
func ClaimsFromContext(c *gin.Context) (claims authapi.Claims, ok bool) {
	if c == nil {
		return authapi.Claims{}, false
	}
	v, exists := c.Get(ClaimsContextKey)
	if !exists {
		return authapi.Claims{}, false
	}
	claims, ok = v.(authapi.Claims)
	return claims, ok
}

// extractBearer parses the Authorization header value and returns the
// token portion when the scheme is "Bearer" (case-insensitive). Returns
// ok=false when the header is empty, has a different scheme, or has no
// token after the scheme.
func extractBearer(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}
	// Split on the first run of whitespace so the token may contain '.'
	// (JWTs are dot-separated) without further parsing.
	scheme, token, found := strings.Cut(header, " ")
	if !found {
		return "", false
	}
	if !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}

// abortUnauthorized writes the canonical 401 envelope and aborts the
// gin chain so downstream handlers do not run.
func abortUnauthorized(c *gin.Context, code, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, errorEnvelope{
		Error:   code,
		Message: message,
	})
}

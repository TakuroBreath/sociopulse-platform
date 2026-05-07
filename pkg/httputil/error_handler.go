package httputil

import "github.com/gin-gonic/gin"

// ErrorEnvelope is the project's HTTP error response shape (per
// docs/architecture/03-error-handling.md § HTTP Errors). Every gin
// handler that fails returns errors via c.Error(err) and the gateway
// renders this envelope.
type ErrorEnvelope struct {
	// Code is the dotted error code, e.g. "auth.invalid_credentials".
	Code string `json:"code"`
	// Message is a human-readable description suitable for a UI toast.
	// It MUST NOT include PII (phone numbers, tokens, etc.).
	Message string `json:"message"`
	// Details optionally carries structured data — e.g. the
	// validation report for surveys.ErrValidation.
	Details map[string]any `json:"details,omitempty"`
}

// ErrorHandler is the gateway-level middleware that drains c.Errors
// after handlers run, maps each error to an ErrorEnvelope + HTTP
// status code (per the table in 03-error-handling.md), and writes the
// response. It also emits sociopulse_http_unmapped_error_total when a
// fallback to 500/internal_error is needed.
func ErrorHandler() gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 3")
}

// RespondError is a convenience for handlers that need to render an
// envelope synchronously rather than queueing via c.Error. Most
// handlers should prefer c.Error(err) so the central handler does the
// mapping in one place.
func RespondError(c *gin.Context, err error) {
	panic("not implemented: see Plan 02 Task 3")
}

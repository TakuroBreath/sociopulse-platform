package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
)

// mapDialerError maps a dialer (or auth-relayed) sentinel to a
// (status, code) pair. The codes are dotted, low-cardinality, and the
// HTTP status follows the project's HTTP error policy:
//
//   - 401 — auth.token_invalid / auth.token_revoked (relayed when
//     the FSM/Router surface a token-related sentinel, rare but possible
//     when claims are revalidated mid-request)
//   - 403 — auth.insufficient_role / dialer.tenant_mismatch
//   - 404 — dialer.queue_empty (when explicitly queried)
//   - 409 — dialer.invalid_transition / dialer.conflict
//   - 422 — dialer.outside_working_hours
//   - 429 — dialer.throttled
//   - 500 — dialer.unknown_state / dialer.internal (default)
//   - 503 — dialer.all_nodes_full
//
// Default branch returns 500/dialer.internal so callers can rely on
// every response carrying a stable code; renderError additionally
// scrubs the message for 5xx so internal details do not leak.
func mapDialerError(err error) (int, string) { //nolint:gocyclo // sentinel switch is intentionally flat for auditability
	switch {
	// Auth sentinels first — RBAC denies and token failures may
	// surface from the requireRole middleware via our error renderer
	// when policy fires.
	case errors.Is(err, authapi.ErrInsufficientRole):
		return http.StatusForbidden, "auth.insufficient_role"
	case errors.Is(err, authapi.ErrTokenInvalid):
		return http.StatusUnauthorized, "auth.token_invalid"
	case errors.Is(err, authapi.ErrTokenRevoked):
		return http.StatusUnauthorized, "auth.token_revoked"

	// Dialer sentinels.
	case errors.Is(err, dialerapi.ErrInvalidTransition):
		return http.StatusConflict, "dialer.invalid_transition"
	case errors.Is(err, dialerapi.ErrConflict):
		return http.StatusConflict, "dialer.conflict"
	case errors.Is(err, dialerapi.ErrTenantMismatch):
		return http.StatusForbidden, "dialer.tenant_mismatch"
	case errors.Is(err, dialerapi.ErrUnknownState):
		return http.StatusInternalServerError, "dialer.unknown_state"
	case errors.Is(err, dialerapi.ErrAllNodesFull):
		return http.StatusServiceUnavailable, "dialer.all_nodes_full"
	case errors.Is(err, dialerapi.ErrOutsideWorkingHours):
		return http.StatusUnprocessableEntity, "dialer.outside_working_hours"
	case errors.Is(err, dialerapi.ErrThrottled):
		return http.StatusTooManyRequests, "dialer.throttled"
	case errors.Is(err, dialerapi.ErrQueueEmpty):
		return http.StatusNotFound, "dialer.queue_empty"
	case errors.Is(err, dialerapi.ErrDuplicateInQueue):
		return http.StatusConflict, "dialer.duplicate_in_queue"

	default:
		return http.StatusInternalServerError, "dialer.internal"
	}
}

// renderError writes the canonical envelope for err. 5xx responses
// scrub the underlying message; the full error is logged at error level
// so ops can triage. 4xx surface the sentinel's public message.
//
// log may be nil in tests; renderError only logs when it is non-nil.
func renderError(c *gin.Context, log *zap.Logger, err error) {
	status, code := mapDialerError(err)
	envelope := ErrorEnvelope{Code: code}
	if status >= http.StatusInternalServerError {
		if log != nil {
			log.Error("dialer/transport/http: internal", zap.Error(err))
		}
		envelope.Message = "internal error"
	} else {
		envelope.Message = err.Error()
	}
	c.AbortWithStatusJSON(status, envelope)
}

// renderBindError surfaces a 400 with a stable code for binding
// failures. Gin's validator errors don't map to api sentinels, so this
// is the canonical landing pad for malformed payloads.
func renderBindError(c *gin.Context, err error) {
	c.AbortWithStatusJSON(http.StatusBadRequest, ErrorEnvelope{
		Code:    "dialer.bad_request",
		Message: err.Error(),
	})
}

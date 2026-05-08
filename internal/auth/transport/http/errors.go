package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/pkg/passwords"
)

// mapAuthError maps an internal sentinel to a (status, code) pair. The
// codes are dotted, low-cardinality, and the HTTP status follows the
// project's HTTP error policy (docs/architecture/03-error-handling.md):
//
//   - 401 — wrong credentials, missing/invalid/revoked token, TOTP gate
//   - 403 — known principal but archived / expired / no permission
//   - 404 — resource missing
//   - 409 — duplicate (login already taken)
//   - 423 — account locked
//   - 429 — rate limit exceeded
//   - 503 — hasher saturated (back-pressure)
//   - 500 — anything unmapped
//
// The default branch returns 500/auth.internal so callers can rely on
// every response carrying a stable code; renderError additionally
// scrubs the message for 5xx so internal details do not leak.
func mapAuthError(err error) (int, string) { //nolint:gocyclo // sentinel switch is intentionally flat for auditability
	switch {
	case errors.Is(err, authapi.ErrInvalidCredentials):
		return http.StatusUnauthorized, "auth.invalid_credentials"
	case errors.Is(err, authapi.ErrAccountLocked):
		return http.StatusLocked, "auth.account_locked"
	case errors.Is(err, authapi.ErrAccountArchived):
		return http.StatusForbidden, "auth.account_archived"
	case errors.Is(err, authapi.ErrPasswordExpired):
		return http.StatusForbidden, "auth.password_expired"
	case errors.Is(err, authapi.ErrTOTPRequired):
		return http.StatusUnauthorized, "auth.totp_required"
	case errors.Is(err, authapi.ErrTOTPInvalid):
		return http.StatusUnauthorized, "auth.totp_invalid"
	case errors.Is(err, authapi.ErrTOTPAlreadyEnabled):
		return http.StatusConflict, "auth.totp_already_enabled"
	case errors.Is(err, authapi.ErrTOTPNotEnrolled):
		return http.StatusBadRequest, "auth.totp_not_enrolled"
	case errors.Is(err, authapi.ErrTokenInvalid):
		return http.StatusUnauthorized, "auth.token_invalid"
	case errors.Is(err, authapi.ErrTokenRevoked):
		return http.StatusUnauthorized, "auth.token_revoked"
	case errors.Is(err, authapi.ErrRefreshReplay):
		return http.StatusUnauthorized, "auth.refresh_replay"
	case errors.Is(err, authapi.ErrRateLimitExceeded):
		return http.StatusTooManyRequests, "auth.rate_limit_exceeded"
	case errors.Is(err, authapi.ErrInsufficientRole):
		return http.StatusForbidden, "auth.insufficient_role"
	case errors.Is(err, authapi.ErrEmptyRoles):
		return http.StatusBadRequest, "auth.empty_roles"
	case errors.Is(err, authapi.ErrLoginTaken):
		return http.StatusConflict, "user.login_taken"
	case errors.Is(err, authapi.ErrUserNotFound):
		return http.StatusNotFound, "user.not_found"
	case errors.Is(err, authapi.ErrUserNotArchived):
		return http.StatusConflict, "user.not_archived"
	case errors.Is(err, passwords.ErrHasherBusy):
		return http.StatusServiceUnavailable, "auth.hasher_busy"
	default:
		return http.StatusInternalServerError, "auth.internal"
	}
}

// renderError writes the canonical envelope for err. 5xx responses do
// not echo the underlying error message — callers see only a generic
// "internal error" string, while the full error is logged at error
// level for ops to triage. 4xx responses include the sentinel's public
// message because that is the user-facing surface.
//
// The supplied logger may be nil in tests; renderError only logs when
// it is non-nil.
func renderError(c *gin.Context, log *zap.Logger, err error) {
	status, code := mapAuthError(err)
	envelope := ErrorEnvelope{Error: code}
	if status >= http.StatusInternalServerError {
		if log != nil {
			log.Error("auth: internal", zap.Error(err))
		}
		envelope.Message = "internal error"
	} else {
		envelope.Message = err.Error()
	}
	c.AbortWithStatusJSON(status, envelope)
}

// renderBindError is a thin convenience for binding failures (gin's
// validator returns errors that don't map to api sentinels). It always
// renders 400 with auth.bad_request so the caller sees a stable code
// even when the validator's error message changes upstream.
func renderBindError(c *gin.Context, err error) {
	c.AbortWithStatusJSON(http.StatusBadRequest, ErrorEnvelope{
		Error:   "auth.bad_request",
		Message: err.Error(),
	})
}

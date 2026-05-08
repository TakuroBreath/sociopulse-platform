package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
)

// mapCRMError maps an internal crm sentinel to a (status, code) pair.
// The codes are dotted, low-cardinality, and the HTTP status follows
// the project's HTTP error policy
// (docs/architecture/03-error-handling.md):
//
//   - 400 — invalid argument / unsupported import format / invalid phone
//   - 401 — token invalid (auth-layer sentinels reflected here too)
//   - 403 — insufficient role / archived project
//   - 404 — project / respondent / import job missing
//   - 409 — duplicate / advertising rejected / import in progress
//   - 410 — respondent already soft-deleted (subject right; row gone)
//   - 413 — import payload too big
//   - 500 — anything unmapped
//
// The default branch returns 500/crm.internal so callers can rely on
// every response carrying a stable code; renderError additionally
// scrubs the message for 5xx so internal details do not leak.
func mapCRMError(err error) (int, string) { //nolint:gocyclo // sentinel switch is intentionally flat for auditability
	switch {
	// Auth sentinels first — RBAC denies and token failures might
	// surface from the requireRole middleware via our error renderer
	// when the policy fires.
	case errors.Is(err, authapi.ErrInsufficientRole):
		return http.StatusForbidden, "auth.insufficient_role"
	case errors.Is(err, authapi.ErrTokenInvalid):
		return http.StatusUnauthorized, "auth.token_invalid"
	case errors.Is(err, authapi.ErrTokenRevoked):
		return http.StatusUnauthorized, "auth.token_revoked"

	// Project sentinels.
	case errors.Is(err, crmapi.ErrProjectNotFound):
		return http.StatusNotFound, "crm.project.not_found"
	case errors.Is(err, crmapi.ErrProjectCodeTaken):
		return http.StatusConflict, "crm.project.code_taken"
	case errors.Is(err, crmapi.ErrProjectArchived):
		return http.StatusConflict, "crm.project.archived"
	case errors.Is(err, crmapi.ErrInvalidStatus):
		return http.StatusConflict, "crm.project.invalid_status"
	case errors.Is(err, crmapi.ErrAdvertisingRejected):
		return http.StatusConflict, "crm.project.advertising_rejected"

	// Respondent sentinels.
	case errors.Is(err, crmapi.ErrRespondentDeleted):
		return http.StatusGone, "crm.respondent.deleted"
	case errors.Is(err, crmapi.ErrRespondentNotFound):
		return http.StatusNotFound, "crm.respondent.not_found"
	case errors.Is(err, crmapi.ErrInvalidPhone):
		return http.StatusBadRequest, "crm.respondent.invalid_phone"
	case errors.Is(err, crmapi.ErrPhoneInDNC):
		return http.StatusConflict, "crm.respondent.phone_in_dnc"
	case errors.Is(err, crmapi.ErrDuplicateRespondent):
		return http.StatusConflict, "crm.respondent.duplicate"

	// Quota / DNC.
	case errors.Is(err, crmapi.ErrInvalidQuotaKind):
		return http.StatusBadRequest, "crm.quota.invalid_kind"

	// Import.
	case errors.Is(err, crmapi.ErrImportInProgress):
		return http.StatusConflict, "crm.import.in_progress"
	case errors.Is(err, crmapi.ErrImportPayloadTooBig):
		return http.StatusRequestEntityTooLarge, "crm.import.payload_too_big"
	case errors.Is(err, crmapi.ErrImportNotFound):
		return http.StatusNotFound, "crm.import.not_found"
	case errors.Is(err, crmapi.ErrImportFormatUnsupported):
		return http.StatusBadRequest, "crm.import.format_unsupported"

	// Generic invalid argument — last so more specific sentinels win.
	case errors.Is(err, crmapi.ErrInvalidArgument):
		return http.StatusBadRequest, "crm.invalid_argument"

	default:
		return http.StatusInternalServerError, "crm.internal"
	}
}

// renderError writes the canonical envelope for err. 5xx responses do
// not echo the underlying error message — callers see only a generic
// "internal error" string, while the full error is logged at error
// level for ops to triage. 4xx responses include the sentinel's
// public message because that is the user-facing surface.
//
// The supplied logger may be nil in tests; renderError only logs when
// it is non-nil.
func renderError(c *gin.Context, log *zap.Logger, err error) {
	status, code := mapCRMError(err)
	envelope := ErrorEnvelope{Error: code}
	if status >= http.StatusInternalServerError {
		if log != nil {
			log.Error("crm: internal", zap.Error(err))
		}
		envelope.Message = "internal error"
	} else {
		envelope.Message = err.Error()
	}
	c.AbortWithStatusJSON(status, envelope)
}

// renderBindError is a thin convenience for binding failures (gin's
// validator returns errors that don't map to api sentinels). It
// always renders 400 with crm.bad_request so the caller sees a stable
// code even when the validator's error message changes upstream.
func renderBindError(c *gin.Context, err error) {
	c.AbortWithStatusJSON(http.StatusBadRequest, ErrorEnvelope{
		Error:   "crm.bad_request",
		Message: err.Error(),
	})
}

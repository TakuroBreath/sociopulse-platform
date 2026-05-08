package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	surveysapi "github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/schemavalidator"
)

// mapSurveyError maps an internal sentinel to a (status, code) pair.
// The codes are dotted, low-cardinality strings safe to log; HTTP
// status follows the project's HTTP error policy
// (docs/architecture/03-error-handling.md):
//
//   - 400 — invalid argument / bad answer
//   - 401 — token invalid (auth-layer sentinels reflected here too)
//   - 403 — insufficient role / archived survey
//   - 404 — survey / version / node missing / no active version
//   - 422 — validation failure (schema, graph) / runtime no-matching-edge
//   - 500 — anything unmapped
//
// Validation-error returns (api.ErrValidation / *api.ValidationError)
// surface a 422 in *both* the standalone "schema is invalid" sense and
// the SaveVersion path. Callers that want the structured report should
// check via errors.As(*api.ValidationError) before handing the error
// to renderError — see saveVersion in handlers.go.
//
// The default branch returns 500/surveys.internal so callers can rely
// on every response carrying a stable code; renderError additionally
// scrubs the message for 5xx so internal details do not leak.
func mapSurveyError(err error) (int, string) { //nolint:gocyclo // sentinel switch is intentionally flat for auditability
	switch {
	// Auth sentinels — surface from middleware / RBAC denial path so
	// the renderer hits the same envelope shape.
	case errors.Is(err, authapi.ErrInsufficientRole):
		return http.StatusForbidden, "auth.insufficient_role"
	case errors.Is(err, authapi.ErrTokenInvalid):
		return http.StatusUnauthorized, "auth.token_invalid"
	case errors.Is(err, authapi.ErrTokenRevoked):
		return http.StatusUnauthorized, "auth.token_revoked"

	// Survey lookup misses.
	case errors.Is(err, surveysapi.ErrVersionNotFound):
		return http.StatusNotFound, "surveys.version_not_found"
	case errors.Is(err, surveysapi.ErrNodeNotFound):
		return http.StatusNotFound, "surveys.node_not_found"
	case errors.Is(err, surveysapi.ErrNoActiveVersion):
		return http.StatusNotFound, "surveys.no_active_version"
	case errors.Is(err, surveysapi.ErrNotFound):
		return http.StatusNotFound, "surveys.not_found"

	// Lifecycle / state.
	case errors.Is(err, surveysapi.ErrSurveyArchived):
		return http.StatusForbidden, "surveys.archived"

	// Validation / graph / DSL.
	case errors.Is(err, surveysapi.ErrCycle),
		errors.Is(err, surveysapi.ErrUnreachable),
		errors.Is(err, surveysapi.ErrDanglingEdge),
		errors.Is(err, surveysapi.ErrForwardRef):
		return http.StatusUnprocessableEntity, "surveys.graph_invalid"
	case errors.Is(err, surveysapi.ErrSchema):
		return http.StatusUnprocessableEntity, "surveys.schema_invalid"
	case errors.Is(err, surveysapi.ErrValidation):
		return http.StatusUnprocessableEntity, "surveys.validation_failed"

	// Runtime.
	case errors.Is(err, surveysapi.ErrNoMatchingEdge):
		return http.StatusUnprocessableEntity, "surveys.no_matching_edge"
	case errors.Is(err, surveysapi.ErrBadAnswer):
		return http.StatusBadRequest, "surveys.bad_answer"

	// Generic invalid argument — last so more specific sentinels win.
	case errors.Is(err, surveysapi.ErrInvalidArgument):
		return http.StatusBadRequest, "surveys.invalid_argument"

	default:
		return http.StatusInternalServerError, "surveys.internal"
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
//
// SaveVersion failures that wrap *api.ValidationError are rendered by
// renderValidationReport instead — that path carries the structured
// issue list, this path carries only the (code, message) envelope.
func renderError(c *gin.Context, log *zap.Logger, err error) {
	status, code := mapSurveyError(err)
	envelope := ErrorEnvelope{Error: code}
	if status >= http.StatusInternalServerError {
		if log != nil {
			log.Error("surveys: internal", zap.Error(err))
		}
		envelope.Message = "internal error"
	} else {
		envelope.Message = err.Error()
	}
	c.AbortWithStatusJSON(status, envelope)
}

// renderBindError is a thin convenience for binding failures (gin's
// validator returns errors that don't map to api sentinels). It
// always renders 400 with surveys.bad_request so the caller sees a
// stable code even when the validator's error message changes
// upstream.
func renderBindError(c *gin.Context, err error) {
	c.AbortWithStatusJSON(http.StatusBadRequest, ErrorEnvelope{
		Error:   "surveys.bad_request",
		Message: err.Error(),
	})
}

// renderValidationReport writes a 422 response carrying a structured
// validation report. Used by both saveVersion (when the service
// returns *api.ValidationError) and validateSchema (the explicit
// preview-validate endpoint). The report is rendered with a stable
// "valid: false" envelope so the wire shape is uniform across both
// callers.
func renderValidationReport(c *gin.Context, report ValidationReportDTO) {
	c.AbortWithStatusJSON(http.StatusUnprocessableEntity, report)
}

// renderValidationReportFromValidator is a convenience wrapper that
// converts a schemavalidator.ValidationReport on the fly. The
// validateSchema handler uses it directly; saveVersion converts via
// apiReportToDTO before reaching this layer.
func renderValidationReportFromValidator(c *gin.Context, report schemavalidator.ValidationReport) {
	renderValidationReport(c, reportToDTO(report))
}

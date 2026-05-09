package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// renderServiceError maps a recording.api sentinel to its HTTP envelope.
// Caller invokes this and returns; the function never panics on nil.
//
// Sentinel → HTTP mapping:
//
//	ErrInvalidInput     → 400 recording.invalid_input
//	ErrNotFound         → 404 recording.not_found
//	ErrAlreadyDeleted   → 410 recording.already_deleted
//	ErrCallNotFound     → 412 recording.call_not_found
//	ErrTenantMismatch   → 403 recording.tenant_mismatch (defence in depth;
//	                       transport-level claims drive the lookup so a
//	                       cross-tenant id is normally caught at the
//	                       store layer as ErrNotFound)
//	default             → 500 recording.internal_error
//
// 5xx response messages are scrubbed; the underlying error stays in the
// caller-supplied logger for ops triage (handlers log before rendering).
func renderServiceError(c *gin.Context, err error) {
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, rapi.ErrInvalidInput):
		c.JSON(http.StatusBadRequest, ErrorEnvelope{
			Code:    "recording.invalid_input",
			Message: err.Error(),
		})
	case errors.Is(err, rapi.ErrNotFound):
		c.JSON(http.StatusNotFound, ErrorEnvelope{
			Code:    "recording.not_found",
			Message: "recording not found",
		})
	case errors.Is(err, rapi.ErrAlreadyDeleted):
		c.JSON(http.StatusGone, ErrorEnvelope{
			Code:    "recording.already_deleted",
			Message: "recording has been deleted",
		})
	case errors.Is(err, rapi.ErrCallNotFound):
		c.JSON(http.StatusPreconditionFailed, ErrorEnvelope{
			Code:    "recording.call_not_found",
			Message: "call not found",
		})
	case errors.Is(err, rapi.ErrTenantMismatch):
		c.JSON(http.StatusForbidden, ErrorEnvelope{
			Code:    "recording.tenant_mismatch",
			Message: "tenant mismatch",
		})
	default:
		c.JSON(http.StatusInternalServerError, ErrorEnvelope{
			Code:    "recording.internal_error",
			Message: "internal server error",
		})
	}
}

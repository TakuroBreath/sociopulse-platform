package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// verifyChecksum handles POST /api/calls/:id/recording/verify.
// Synchronously fetches the ciphertext and recomputes its sha256.
// 200 OK with VerifyResponse on success (OK=true if matches; OK=false
// if doesn't — both are 200 because the verify itself succeeded).
//
// Mismatched sha (OK=false) is the canonical signal for the operator
// to investigate; we don't 5xx because the recording exists and the
// verify completed — the storage layer just delivered different bytes
// than what was committed.
func (h *handlers) verifyChecksum(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}

	callID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorEnvelope{
			Code:    "recording.invalid_input",
			Message: "call_id must be a UUID",
		})
		return
	}

	result, err := h.d.Service.VerifyChecksum(c.Request.Context(), claims.TenantID, callID)
	if err != nil {
		renderServiceError(c, err)
		return
	}

	c.JSON(http.StatusOK, VerifyResponse{
		OK:           result.OK,
		ExpectedSHA:  result.ExpectedSHA,
		ActualSHA:    result.ActualSHA,
		BytesScanned: result.BytesScanned,
		DurationMS:   result.DurationMS,
	})
}

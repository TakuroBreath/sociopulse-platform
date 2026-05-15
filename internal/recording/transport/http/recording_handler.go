package http

import (
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// streamRecording handles GET /api/calls/:id/recording.
// Streams the decrypted plaintext audio to the response body.
//
// v1 trade-off: Accept-Ranges: none. Plan 12.2's OpenAudioStream buffers
// the entire plaintext in RAM before returning, so the response is a
// single contiguous chunk. v2 chunked-envelope (deferred) will support
// Range / partial content.
//
// Cache-Control: private, no-store — recording payloads are chain-of-custody
// material and must not leak into intermediate caches.
func (h *handlers) streamRecording(c *gin.Context) {
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

	stream, err := h.d.Service.OpenAudioStream(c.Request.Context(), claims.TenantID, callID, nil)
	if err != nil {
		renderServiceError(c, err)
		return
	}
	defer stream.Reader.Close()

	c.Header("Content-Type", stream.ContentType)
	c.Header("Content-Length", strconv.FormatInt(stream.ContentLength, 10))
	c.Header("Accept-Ranges", "none")
	c.Header("Cache-Control", "private, no-store")

	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, stream.Reader); err != nil {
		// Connection broke mid-write; status is already 200 so we can't
		// signal the failure to the client. Log only.
		if h.d.Logger != nil {
			h.d.Logger.Warn("recording stream interrupted",
				zap.String("call_id", callID.String()),
				zap.Error(err))
		}
	}
}

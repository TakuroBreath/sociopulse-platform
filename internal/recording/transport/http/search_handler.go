package http

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// searchRecordings handles GET /api/recordings/search.
// Query params (all optional):
//
//	project_id  uuid
//	operator_id uuid
//	status      comma-separated subset of {stored,cold,deleted}
//	from        RFC3339 timestamp (inclusive)
//	to          RFC3339 timestamp (exclusive)
//	cursor      opaque base64 from previous page's next_cursor
//	limit       1..200, default 50
func (h *handlers) searchRecordings(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}

	q, err := parseSearchQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorEnvelope{
			Code:    "recording.invalid_input",
			Message: err.Error(),
		})
		return
	}

	result, err := h.d.Service.Search(c.Request.Context(), claims.TenantID, q)
	if err != nil {
		renderServiceError(c, err)
		return
	}

	resp := SearchResponse{
		Items:      make([]RecordingMetadataDTO, 0, len(result.Items)),
		NextCursor: result.NextCursor,
		HasMore:    result.HasMore,
	}
	for _, m := range result.Items {
		resp.Items = append(resp.Items, RecordingMetadataDTO{
			RecordingID: m.RecordingID,
			CallID:      m.CallID,
			TenantID:    m.TenantID,
			BytesSize:   m.BytesSize,
			DurationMS:  m.Duration.Milliseconds(),
			SHA256Hex:   m.SHA256Hex,
			Status:      m.Status,
			CommittedAt: m.CommittedAt,
			DeleteAt:    m.DeleteAt,
			ColdAt:      m.ColdAt,
			VerifiedAt:  m.VerifiedAt,
		})
	}
	c.JSON(http.StatusOK, resp)
}

// parseSearchQuery extracts and validates query params. Empty params
// yield nil pointers / zero values which the service treats as "no
// filter". Each branch is a Query-then-Parse pair; cyclomatic complexity
// is bounded by the number of supported params.
func parseSearchQuery(c *gin.Context) (rapi.SearchQuery, error) {
	q := rapi.SearchQuery{}

	if v := c.Query("project_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("project_id: %w", err)
		}
		q.ProjectID = &id
	}
	if v := c.Query("operator_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("operator_id: %w", err)
		}
		q.OperatorID = &id
	}
	if v := c.Query("status"); v != "" {
		q.Status = strings.Split(v, ",")
	}
	if v := c.Query("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("from: %w", err)
		}
		q.From = &t
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("to: %w", err)
		}
		q.To = &t
	}
	q.Cursor = c.Query("cursor")
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("limit: %w", err)
		}
		q.Limit = n
	}
	return q, nil
}

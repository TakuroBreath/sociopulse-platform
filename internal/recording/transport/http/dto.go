package http

import (
	"time"

	"github.com/google/uuid"
)

// ErrorEnvelope is the project-wide error response shape.
type ErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SearchResponse is the paginated /api/recordings/search payload.
type SearchResponse struct {
	Items      []RecordingMetadataDTO `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
	HasMore    bool                   `json:"has_more"`
}

// RecordingMetadataDTO is the JSON projection of api.RecordingMetadata.
// Field names use snake_case per project convention. DurationMS is
// milliseconds (consistent with call_recordings.duration_ms column).
type RecordingMetadataDTO struct {
	RecordingID uuid.UUID `json:"recording_id"`
	CallID      uuid.UUID `json:"call_id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	BytesSize   int64     `json:"bytes_size"`
	DurationMS  int64     `json:"duration_ms"`
	SHA256Hex   string    `json:"sha256"`
	Status      string    `json:"status"`
	CommittedAt time.Time `json:"committed_at"`
	// time.Time with `omitempty` does NOT omit a zero time — `omitempty` only
	// drops Go zero values for primitives. Use `omitzero` (Go 1.24+ json tag)
	// which calls IsZero() on time.Time and correctly drops "0001-01-01..."
	// from the wire when the row carries NULL delete_at (legal-hold scenarios).
	DeleteAt   time.Time  `json:"delete_at,omitzero"`
	ColdAt     time.Time  `json:"cold_at"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

// VerifyResponse is the POST /verify payload.
type VerifyResponse struct {
	OK           bool   `json:"ok"`
	ExpectedSHA  string `json:"expected_sha"`
	ActualSHA    string `json:"actual_sha"`
	BytesScanned int64  `json:"bytes_scanned"`
	DurationMS   int64  `json:"duration_ms"`
}

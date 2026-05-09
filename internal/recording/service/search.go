package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/store"
)

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 200
)

// cursor is the wire-format intermediate between SearchQuery.Cursor (opaque
// string) and the store's keyset position. Encoded as base64-url JSON so
// the API consumer can treat it as opaque.
type cursor struct {
	CommittedAt time.Time `json:"c"`
	ID          uuid.UUID `json:"i"`
}

// encodeCursor returns the URL-safe base64 of {committed_at, id} JSON.
// Empty input (zero time + nil UUID) returns the empty string — used when
// HasMore=false to signal "no next page".
func encodeCursor(committedAt time.Time, id uuid.UUID) string {
	if committedAt.IsZero() && id == uuid.Nil {
		return ""
	}
	payload, _ := json.Marshal(cursor{CommittedAt: committedAt, ID: id})
	return base64.URLEncoding.EncodeToString(payload)
}

// decodeCursor parses the wire string. Empty string yields zero values
// (interpreted as "first page" by the store). Malformed input is folded
// into ErrInvalidInput so the HTTP layer maps to 400.
func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	if s == "" {
		return time.Time{}, uuid.Nil, nil
	}
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor decode: %s", ErrInvalidInput, err.Error())
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor unmarshal: %s", ErrInvalidInput, err.Error())
	}
	if c.CommittedAt.IsZero() || c.ID == uuid.Nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor missing committed_at or id", ErrInvalidInput)
	}
	return c.CommittedAt, c.ID, nil
}

// validateSearchStatus rejects status values outside the schema check
// constraint. The store SQL would 22P02 on bad values; we surface
// ErrInvalidInput up-front for a cleaner client error.
func validateSearchStatus(status []string) error {
	allowed := map[string]struct{}{"stored": {}, "cold": {}, "deleted": {}}
	for _, st := range status {
		if _, ok := allowed[st]; !ok {
			return fmt.Errorf("%w: status %q not in {stored,cold,deleted}", ErrInvalidInput, st)
		}
	}
	return nil
}

// rowToMetadata is the canonical store→api projection. Mirrors the same
// mapping previously inlined in svc.Get; svc.Search joins this so the wire
// representation is consistent across /api/calls/:id/recording (single-record),
// /api/recordings/search (page items), and gRPC RecordingService.Get
// (Plan 12.1).
func rowToMetadata(r store.RecordingRow) rapi.RecordingMetadata {
	var deleteAt time.Time
	if r.DeleteAt != nil {
		deleteAt = *r.DeleteAt
	}
	return rapi.RecordingMetadata{
		RecordingID:    r.ID,
		CallID:         r.CallID,
		TenantID:       r.TenantID,
		S3Bucket:       r.S3Bucket,
		AudioObjectKey: r.AudioObjectKey,
		BytesSize:      r.BytesSize,
		Duration:       time.Duration(r.DurationMS) * time.Millisecond,
		SHA256Hex:      r.SHA256Hex,
		Status:         r.Status,
		CommittedAt:    r.CommittedAt,
		DeleteAt:       deleteAt,
		ColdAt:         r.ColdAt,
		VerifiedAt:     r.VerifiedAt,
	}
}

// Search satisfies rapi.RecordingService. Maps the public SearchQuery to
// the store-layer SearchQ, normalises the limit, decodes the cursor, calls
// the store, and packs results into SearchResult including the next cursor.
//
// Limit normalisation: 0 → 50 (default per dto.go); >200 → clamped to 200.
//
// Pagination semantics: the store is asked for Limit+1 rows; if it returns
// Limit+1 we set HasMore=true and trim the last row to compute NextCursor
// from the LAST RETURNED row (not the trimmed extra). HasMore=false means
// the page is exhaustive — NextCursor is empty.
func (s *svc) Search(ctx context.Context, tenantID uuid.UUID, q rapi.SearchQuery) (rapi.SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	if err := validateSearchStatus(q.Status); err != nil {
		return rapi.SearchResult{}, err
	}

	cursorCA, cursorID, err := decodeCursor(q.Cursor)
	if err != nil {
		return rapi.SearchResult{}, err
	}

	// Peek one extra row to detect HasMore. Cap at maxSearchLimit so the store
	// never sees Limit > 200 (it rejects such values). At limit==200 we forgo
	// the peek — HasMore will be false even if a 201st row exists, which is an
	// acceptable trade-off at the API's maximum page size.
	peekLimit := limit + 1
	if peekLimit > maxSearchLimit {
		peekLimit = maxSearchLimit
	}
	storeQ := store.SearchQ{
		ProjectID:  q.ProjectID,
		OperatorID: q.OperatorID,
		Status:     q.Status,
		From:       q.From,
		To:         q.To,
		Limit:      peekLimit,
	}
	if !cursorCA.IsZero() {
		storeQ.CursorCommittedAt = &cursorCA
		storeQ.CursorRecordingID = &cursorID
	}

	rows, err := s.store.Search(ctx, tenantID, storeQ)
	if err != nil {
		return rapi.SearchResult{}, fmt.Errorf("recording.search: %w", err)
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit] // trim the peek row
	}

	items := make([]rapi.RecordingMetadata, 0, len(rows))
	for _, r := range rows {
		items = append(items, rowToMetadata(r))
	}

	nextCursor := ""
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		nextCursor = encodeCursor(last.CommittedAt, last.ID)
	}

	return rapi.SearchResult{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// SearchQ is the cursor-paginated input to PostgresStore.Search.
// All pointers are optional filters; nil/zero means "no filter on this dimension".
// CursorCommittedAt+CursorRecordingID together encode the position from a
// previous page — both must be non-zero (or both zero for first page).
type SearchQ struct {
	ProjectID         *uuid.UUID
	OperatorID        *uuid.UUID
	Status            []string
	From              *time.Time
	To                *time.Time
	CursorCommittedAt *time.Time
	CursorRecordingID *uuid.UUID
	Limit             int
}

// Search returns up to q.Limit rows matching the filter. Rows are ordered by
// (committed_at DESC, id DESC) — same order as the supporting index from
// migration 000010. Pagination is keyset-style via (CursorCommittedAt,
// CursorRecordingID); pass both as nil for the first page.
//
// Caller-side cursor encoding lives in service.Search — the store stays
// SQL-only.
//
// Implementation note: the JOIN with `calls` is only added when project_id
// or operator_id filtering is requested. The join is one-to-one (calls.id
// is PK; call_recordings.call_id is UNIQUE) so the planner satisfies it
// via a nested-loop index scan.
func (s *PostgresStore) Search(ctx context.Context, tenantID uuid.UUID, q SearchQ) ([]RecordingRow, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		return nil, fmt.Errorf("recording.store: search limit must be 1..200; got %d", q.Limit)
	}
	if (q.CursorCommittedAt == nil) != (q.CursorRecordingID == nil) {
		return nil, errors.New("recording.store: cursor requires BOTH committed_at AND recording_id")
	}

	sql, args := buildSearchSQL(tenantID, q)

	var rows []RecordingRow
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		r, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("recording.store: search query: %w", err)
		}
		defer r.Close()

		for r.Next() {
			var row RecordingRow
			if scanErr := r.Scan(
				&row.ID, &row.CallID, &row.TenantID, &row.S3Bucket, &row.AudioObjectKey, &row.DEKObjectKey,
				&row.KMSKeyID, &row.EncryptedDEK, &row.BytesSize, &row.DurationMS, &row.SHA256Hex,
				&row.Codec, &row.SampleRate, &row.Status, &row.CommittedAt, &row.DeleteAt, &row.ColdAt,
				&row.RecordedAt, &row.VerifiedAt, &row.IntegrityOK, &row.IngestAgentID,
			); scanErr != nil {
				return fmt.Errorf("recording.store: scan: %w", scanErr)
			}
			rows = append(rows, row)
		}
		return r.Err()
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// buildSearchSQL constructs the SQL string + parameter slice. Split into a
// helper so each test can assert SQL shape independently of pgx driver.
func buildSearchSQL(tenantID uuid.UUID, q SearchQ) (string, []any) {
	args := []any{tenantID}
	whereTerms := []string{"cr.tenant_id = $1"}

	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	needsJoin := q.ProjectID != nil || q.OperatorID != nil
	if q.ProjectID != nil {
		whereTerms = append(whereTerms, "c.project_id = "+addArg(*q.ProjectID))
	}
	if q.OperatorID != nil {
		whereTerms = append(whereTerms, "c.operator_id = "+addArg(*q.OperatorID))
	}
	if len(q.Status) > 0 {
		whereTerms = append(whereTerms, "cr.status = ANY("+addArg(q.Status)+")")
	}
	if q.From != nil {
		whereTerms = append(whereTerms, "cr.committed_at >= "+addArg(*q.From))
	}
	if q.To != nil {
		whereTerms = append(whereTerms, "cr.committed_at < "+addArg(*q.To))
	}
	if q.CursorCommittedAt != nil && q.CursorRecordingID != nil {
		// Tuple compare — both columns indexed DESC, so strictly-less-than
		// gives us the next page.
		whereTerms = append(whereTerms,
			"(cr.committed_at, cr.id) < ("+addArg(*q.CursorCommittedAt)+", "+addArg(*q.CursorRecordingID)+")")
	}

	join := ""
	if needsJoin {
		join = "JOIN calls c ON c.id = cr.call_id"
	}

	limitArg := addArg(q.Limit)

	sql := fmt.Sprintf(`
SELECT cr.id, cr.call_id, cr.tenant_id, cr.s3_bucket, cr.audio_object_key, cr.dek_object_key,
       cr.kms_key_id, cr.encrypted_dek, cr.bytes_size, cr.duration_ms, cr.sha256_hex,
       cr.codec, cr.sample_rate, cr.status, cr.committed_at, cr.delete_at, cr.cold_at,
       cr.recorded_at, cr.verified_at, cr.integrity_ok, cr.ingest_agent_id
FROM call_recordings cr
%s
WHERE %s
ORDER BY cr.committed_at DESC, cr.id DESC
LIMIT %s
`, join, joinAnd(whereTerms), limitArg)
	return sql, args
}

func joinAnd(terms []string) string {
	return strings.Join(terms, " AND ")
}

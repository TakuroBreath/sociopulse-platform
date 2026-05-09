package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// clampLifecycleLimit normalises a worker batch size to [1, 1000].
// 1000 is the same upper bound used by dialer/retry's mature-row sweep —
// it caps lock-table pressure on the underlying SELECT and keeps a single
// worker tick bounded.
func clampLifecycleLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

// ListDueColdMoves implements rapi.LifecycleStore. It runs cross-tenant
// inside a BypassRLS transaction so the privileged retention worker sees
// every tenant's rows in one pass.
//
// Predicate: status='stored' AND cold_at <= $now. Ordered by cold_at ASC
// so the oldest backlog drains first. Limit is clamped to [1, 1000].
//
// The supporting partial index call_recordings_status_cold_at_idx
// (migrations/000010) covers (status, cold_at) WHERE status='stored',
// keeping the planner on an index range scan.
func (s *PostgresStore) ListDueColdMoves(ctx context.Context, now time.Time, limit int) ([]rapi.LifecycleRow, error) {
	limit = clampLifecycleLimit(limit)

	const q = `
SELECT id, tenant_id, call_id, s3_bucket, audio_object_key, sha256_hex,
       status, cold_at, delete_at
  FROM call_recordings
 WHERE status = 'stored'
   AND cold_at <= $1
 ORDER BY cold_at ASC
 LIMIT $2
`
	var out []rapi.LifecycleRow
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, q, now, limit)
		if err != nil {
			return fmt.Errorf("recording.store: list due cold moves: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var row rapi.LifecycleRow
			if scanErr := rows.Scan(
				&row.ID, &row.TenantID, &row.CallID,
				&row.S3Bucket, &row.AudioObjectKey, &row.SHA256Hex,
				&row.Status, &row.ColdAt, &row.DeleteAt,
			); scanErr != nil {
				return fmt.Errorf("recording.store: scan cold-move row: %w", scanErr)
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Ensure PostgresStore satisfies the LifecycleStore contract. The full
// compile-time check at the bottom of this file verifies the entire
// interface is implemented; this file's incremental commits leave that
// assertion in place once every method exists.
var _ interface {
	ListDueColdMoves(ctx context.Context, now time.Time, limit int) ([]rapi.LifecycleRow, error)
} = (*PostgresStore)(nil)

// _ keeps uuid imported until the remaining methods land in this file.
var _ uuid.UUID

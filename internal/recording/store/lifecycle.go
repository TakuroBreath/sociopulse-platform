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

// ListDueDeletes implements rapi.LifecycleStore. It runs cross-tenant
// inside a BypassRLS transaction.
//
// Predicate: status IN ('stored','cold') AND delete_at IS NOT NULL AND
// delete_at <= $now. The IS NOT NULL clause excludes legal-hold rows
// (where delete_at is null = "no scheduled deletion"). Already-deleted
// rows are excluded because there is no S3 object left to purge.
//
// Ordered by delete_at ASC so the oldest backlog drains first. The
// supporting partial index call_recordings_status_delete_at_idx
// (migrations/000010) covers (status, delete_at) WHERE status IN
// ('stored','cold').
func (s *PostgresStore) ListDueDeletes(ctx context.Context, now time.Time, limit int) ([]rapi.LifecycleRow, error) {
	limit = clampLifecycleLimit(limit)

	const q = `
SELECT id, tenant_id, call_id, s3_bucket, audio_object_key, sha256_hex,
       status, cold_at, delete_at
  FROM call_recordings
 WHERE status IN ('stored', 'cold')
   AND delete_at IS NOT NULL
   AND delete_at <= $1
 ORDER BY delete_at ASC
 LIMIT $2
`
	var out []rapi.LifecycleRow
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, q, now, limit)
		if err != nil {
			return fmt.Errorf("recording.store: list due deletes: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var row rapi.LifecycleRow
			if scanErr := rows.Scan(
				&row.ID, &row.TenantID, &row.CallID,
				&row.S3Bucket, &row.AudioObjectKey, &row.SHA256Hex,
				&row.Status, &row.ColdAt, &row.DeleteAt,
			); scanErr != nil {
				return fmt.Errorf("recording.store: scan delete row: %w", scanErr)
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

// MarkCold implements rapi.LifecycleStore. Status-CAS pattern: the WHERE
// clause requires status='stored', so a concurrent worker that already
// moved the row to 'cold' (or 'deleted') causes this UPDATE to match
// zero rows. Returns rowsAffected — caller treats 0 as a benign skip.
//
// The method runs cross-tenant via BypassRLS; the worker that calls it
// already verified the row's existence via ListDueColdMoves on the same
// tick.
func (s *PostgresStore) MarkCold(ctx context.Context, id uuid.UUID) (int64, error) {
	const q = `
UPDATE call_recordings
   SET status = 'cold'
 WHERE id = $1
   AND status = 'stored'
`
	var rowsAffected int64
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		tag, execErr := tx.Exec(ctx, q, id)
		if execErr != nil {
			return fmt.Errorf("recording.store: mark cold %s: %w", id, execErr)
		}
		rowsAffected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rowsAffected, nil
}

// MarkDeleted implements rapi.LifecycleStore. Status-CAS pattern: the
// WHERE clause requires status IN ('stored','cold'), so a concurrent
// delete causes this UPDATE to match zero rows. Returns rowsAffected.
//
// Note: this only flips the status column. The caller (retention worker)
// is responsible for purging the underlying S3 object — the row is kept
// as an audit trail until a future archival migration trims it.
func (s *PostgresStore) MarkDeleted(ctx context.Context, id uuid.UUID) (int64, error) {
	const q = `
UPDATE call_recordings
   SET status = 'deleted'
 WHERE id = $1
   AND status IN ('stored', 'cold')
`
	var rowsAffected int64
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		tag, execErr := tx.Exec(ctx, q, id)
		if execErr != nil {
			return fmt.Errorf("recording.store: mark deleted %s: %w", id, execErr)
		}
		rowsAffected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rowsAffected, nil
}

// clampSamplePct normalises the BERNOULLI sample percent to [0.01, 100].
// A value at or below the floor is raised to 0.01 (the spec contract's
// minimum — small enough for a billion-row table while staying well above
// BERNOULLI's "must be between 0 and 100" error), and values above 100
// are clamped to 100 (= scan the entire table).
func clampSamplePct(pct float64) float64 {
	if pct < 0.01 {
		return 0.01
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// SampleForVerify implements rapi.LifecycleStore. The query uses
// TABLESAMPLE BERNOULLI for a single-pass O(n) skip-filter — at 36.5M
// row scale (50k calls/day × 730d retention) ORDER BY random() is
// O(n log n) and prohibitively slow.
//
// Eligibility: status IN ('stored','cold') AND (verified_at IS NULL OR
// verified_at < now() - interval '7 days'). status='deleted' is excluded
// (no S3 object to verify); recently-verified rows are excluded so verify
// load spreads evenly across the week.
//
// Note: TABLESAMPLE samples the table BEFORE the WHERE clause runs, so
// the post-filter result size is approximately
// `eligible_rows * (samplePct / 100)` and is not deterministic. Tests
// rely on samplePct=100 to exhaust the eligible set.
func (s *PostgresStore) SampleForVerify(ctx context.Context, samplePct float64, limit int) ([]rapi.LifecycleRow, error) {
	limit = clampLifecycleLimit(limit)
	samplePct = clampSamplePct(samplePct)

	// TABLESAMPLE BERNOULLI accepts a parameter placeholder for the
	// percent. The aliased table name keeps the WHERE column references
	// readable; the sampler still scans only `call_recordings`.
	const q = `
SELECT cr.id, cr.tenant_id, cr.call_id, cr.s3_bucket, cr.audio_object_key, cr.sha256_hex,
       cr.status, cr.cold_at, cr.delete_at
  FROM call_recordings cr TABLESAMPLE BERNOULLI ($1)
 WHERE cr.status IN ('stored', 'cold')
   AND (cr.verified_at IS NULL OR cr.verified_at < now() - interval '7 days')
 LIMIT $2
`
	var out []rapi.LifecycleRow
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, q, samplePct, limit)
		if err != nil {
			return fmt.Errorf("recording.store: sample for verify: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var row rapi.LifecycleRow
			if scanErr := rows.Scan(
				&row.ID, &row.TenantID, &row.CallID,
				&row.S3Bucket, &row.AudioObjectKey, &row.SHA256Hex,
				&row.Status, &row.ColdAt, &row.DeleteAt,
			); scanErr != nil {
				return fmt.Errorf("recording.store: scan verify-sample row: %w", scanErr)
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

// UpdateVerifyResult implements rapi.LifecycleStore. It writes
// verified_at=verifiedAt and integrity_ok=ok unconditionally — re-applying
// with the same (id, verifiedAt, ok) is a benign overwrite.
//
// verifiedAt is supplied by the caller (rather than via SQL now()) so the
// integrity worker can compute time.Now() once and apply the same instant
// to both this row's verified_at column and the paired audit-log row's ts
// column, preserving the chain-of-custody coherence contract.
//
// The method does NOT distinguish "row not found" from "row updated": the
// integrity verifier always operates on rows it just selected via
// SampleForVerify in the same tick, so a missing row is a logic bug
// (concurrent admin delete) and surfacing it as an error here would
// trigger spurious worker alerts. The caller asserting "I just selected
// this id" is the contract.
func (s *PostgresStore) UpdateVerifyResult(ctx context.Context, id uuid.UUID, verifiedAt time.Time, ok bool) error {
	const q = `
UPDATE call_recordings
   SET verified_at = $2,
       integrity_ok = $3
 WHERE id = $1
`
	return s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		if _, err := tx.Exec(ctx, q, id, verifiedAt, ok); err != nil {
			return fmt.Errorf("recording.store: update verify result %s: %w", id, err)
		}
		return nil
	})
}

// MarkColdTx is the in-Tx variant of MarkCold. Status-CAS pattern: the
// WHERE clause requires status='stored', returning rowsAffected so the
// caller treats 0 as a benign skip.
//
// Tx-scope contract: the caller MUST have set the tenant scope via
// pool.WithTenant before invoking — MarkColdTx does NOT switch role
// itself. Using this from a BypassRLS Tx will fail unless the row
// happens to satisfy the tenant_id RLS predicate.
//
// The retention worker uses this so MarkCold + audit insert commit
// atomically inside one WithTenant transaction. Splitting them across
// two transactions would let the audit row leak even when the status
// flip rolled back, leaving a divergent chain-of-custody record.
func (s *PostgresStore) MarkColdTx(ctx context.Context, tx postgres.Tx, id uuid.UUID) (int64, error) {
	const q = `
UPDATE call_recordings
   SET status = 'cold'
 WHERE id = $1
   AND status = 'stored'
`
	tag, err := tx.Exec(ctx, q, id)
	if err != nil {
		return 0, fmt.Errorf("recording.store: mark cold tx %s: %w", id, err)
	}
	return tag.RowsAffected(), nil
}

// MarkDeletedTx is the in-Tx variant of MarkDeleted. Status-CAS pattern:
// the WHERE clause requires status IN ('stored','cold'), returning
// rowsAffected so the caller treats 0 as a benign skip (concurrent
// delete already happened).
//
// Tx-scope contract: the caller MUST have set the tenant scope via
// pool.WithTenant before invoking — MarkDeletedTx does NOT switch role
// itself. The retention worker batches MarkDeletedTx with audit_log +
// event_outbox inserts inside one WithTenant transaction so all three
// state changes commit atomically; an S3 delete that already happened
// (Phase A) is reconciled with DB state (Phase B) in a single Tx.
//
// Note: this only flips the status column. The audio object purge is
// the worker's Phase A (irreversible) responsibility — see
// internal/recording/worker.RetentionPass.
func (s *PostgresStore) MarkDeletedTx(ctx context.Context, tx postgres.Tx, id uuid.UUID) (int64, error) {
	const q = `
UPDATE call_recordings
   SET status = 'deleted'
 WHERE id = $1
   AND status IN ('stored', 'cold')
`
	tag, err := tx.Exec(ctx, q, id)
	if err != nil {
		return 0, fmt.Errorf("recording.store: mark deleted tx %s: %w", id, err)
	}
	return tag.RowsAffected(), nil
}

// Compile-time check that *PostgresStore satisfies rapi.LifecycleStore.
// This is the canonical assertion — failing here means a method was
// removed or its signature drifted.
var _ rapi.LifecycleStore = (*PostgresStore)(nil)

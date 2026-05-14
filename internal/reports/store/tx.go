package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sociopulse/platform/pkg/postgres"
)

// ErrStaleSkip is returned by *Tx state-flip methods when
// RowsAffected()==0 — the row was already in a terminal state, or the
// WHERE clause's state filter excluded it. Caller treats this as a
// benign skip: the asynq consumer acks the task and exits the handler
// without further side effects. Mirrors the errStaleSkip pattern Plan
// 12.4 established for the recording lifecycle workers.
//
// The sentinel is returned bare (not wrapped) so callers can compare
// with errors.Is(err, ErrStaleSkip) without unwrapping.
var ErrStaleSkip = errors.New("reports.store: stale state-flip skipped")

// MarkRunningTx transitions a job from 'queued' to 'running' and stamps
// started_at. The CAS predicate `state = 'queued'` is the idempotency
// guard: a retried asynq task that lost a race against the original
// handler hits zero rows affected and the caller skips via
// ErrStaleSkip.
//
// Tx-scope contract: the caller MUST have set the tenant scope via
// pool.WithTenant before invoking. Tx itself does not switch role.
func MarkRunningTx(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time) error {
	const q = `
UPDATE reports_jobs
   SET state = 'running',
       started_at = $2
 WHERE id = $1
   AND state = 'queued'
`
	tag, err := tx.Exec(ctx, q, jobID, ts)
	if err != nil {
		return fmt.Errorf("reports.store.MarkRunningTx: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleSkip
	}
	return nil
}

// MarkSucceededTx finalises a job: state→'succeeded', finished_at,
// bytes_size, filename, download_url all written atomically. The CAS
// predicate `state IN ('queued','running')` rejects already-terminal
// rows (succeeded/failed/canceled) with ErrStaleSkip, so a duplicate
// task processor cannot overwrite a prior outcome.
//
// download_url is the 24h-TTL presigned S3 URL; the consumer signs it
// just before calling this method so the TTL window starts at the
// instant the row is committed.
//
// Tx-scope contract: caller MUST have set tenant scope via
// pool.WithTenant.
func MarkSucceededTx(
	ctx context.Context,
	tx postgres.Tx,
	jobID string,
	ts time.Time,
	bytesSize int64,
	filename string,
	presignedURL string,
) error {
	const q = `
UPDATE reports_jobs
   SET state = 'succeeded',
       finished_at = $2,
       bytes_size = $3,
       filename = $4,
       download_url = $5
 WHERE id = $1
   AND state IN ('queued', 'running')
`
	tag, err := tx.Exec(ctx, q, jobID, ts, bytesSize, filename, presignedURL)
	if err != nil {
		return fmt.Errorf("reports.store.MarkSucceededTx: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleSkip
	}
	return nil
}

// MarkFailedTx finalises a job with state='failed', finished_at, and a
// caller-supplied error message (low-cardinality preferred: log
// aggregators index on it). CAS predicate excludes already-terminal
// rows — duplicate-failure retries return ErrStaleSkip.
//
// errMsg is written verbatim; the caller is responsible for keeping it
// PII-free and bounded in length.
//
// Tx-scope contract: caller MUST have set tenant scope via
// pool.WithTenant.
func MarkFailedTx(
	ctx context.Context,
	tx postgres.Tx,
	jobID string,
	ts time.Time,
	errMsg string,
) error {
	const q = `
UPDATE reports_jobs
   SET state = 'failed',
       finished_at = $2,
       error = $3
 WHERE id = $1
   AND state IN ('queued', 'running')
`
	tag, err := tx.Exec(ctx, q, jobID, ts, errMsg)
	if err != nil {
		return fmt.Errorf("reports.store.MarkFailedTx: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleSkip
	}
	return nil
}

// MarkCanceledTx finalises a job with state='canceled' and a fixed
// 'canceled by caller' error literal so the dashboard can surface the
// distinction without joining against an audit table. CAS predicate
// excludes already-terminal rows.
//
// Tx-scope contract: caller MUST have set tenant scope via
// pool.WithTenant.
func MarkCanceledTx(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time) error {
	const q = `
UPDATE reports_jobs
   SET state = 'canceled',
       finished_at = $2,
       error = 'canceled by caller'
 WHERE id = $1
   AND state IN ('queued', 'running')
`
	tag, err := tx.Exec(ctx, q, jobID, ts)
	if err != nil {
		return fmt.Errorf("reports.store.MarkCanceledTx: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleSkip
	}
	return nil
}

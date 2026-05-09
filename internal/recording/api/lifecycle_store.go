package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// LifecycleRow is the cross-tenant projection of a call_recordings row that
// lifecycle workers consume. Workers (retention sweep, integrity verifier)
// read these via LifecycleStore methods that run with BYPASSRLS so that one
// privileged sweep can see every tenant's rows in a single pass.
//
// Field set is the minimum required by the workers; private columns (DEK
// material, codec, ingest agent, etc.) stay inside the store package and
// are deliberately omitted here. A worker that needs more fields should
// fetch the full RecordingMetadata via the per-tenant audited path.
type LifecycleRow struct {
	// ID is call_recordings.id (the surrogate PK introduced by Plan 12.1).
	ID uuid.UUID
	// TenantID is required so workers can scope follow-up actions per tenant
	// (audit emission, S3 client selection, etc.).
	TenantID uuid.UUID
	// CallID is the parent call's id; used by audit log and event payloads.
	CallID uuid.UUID
	// S3Bucket is the bucket the audio object lives in.
	S3Bucket string
	// AudioObjectKey is the S3 key of the encrypted audio object.
	AudioObjectKey string
	// SHA256Hex is the hex-encoded sha256 of the *plaintext* audio. The
	// integrity verifier recomputes the digest of the decrypted stream and
	// compares against this value.
	SHA256Hex string
	// Status is one of {"stored","cold","deleted"} — the worker filters
	// further before acting.
	Status string
	// ColdAt is the timestamp at which the row becomes eligible for the
	// hot→cold tier transition (S3 storage class change).
	ColdAt time.Time
	// DeleteAt is the scheduled hard-delete timestamp. NULL (nil) means
	// "no scheduled deletion" — typically a legal-hold row that the
	// retention worker must skip.
	DeleteAt *time.Time
}

// LifecycleStore is the persistence surface for the recording-module
// lifecycle workers (retention sweep + integrity sampler). Every method
// runs cross-tenant via postgres.Pool.BypassRLS — production callers are
// privileged background workers, never per-request handlers.
//
// Implementations MUST be safe to share across goroutines (the canonical
// PostgresStore implementation is stateless and a single pointer is reused
// by every worker tick).
//
// MarkCold and MarkDeleted use a status-CAS (compare-and-set) pattern: the
// SQL WHERE clause includes the expected current status, and the methods
// return the rowsAffected. A return value of 0 means another worker (or a
// concurrent admin operation) already moved the row past that status; the
// caller treats that as a benign skip rather than an error.
type LifecycleStore interface {
	// ListDueColdMoves returns up to `limit` rows whose status='stored'
	// and cold_at <= now. The rows are ordered by cold_at ascending so
	// the oldest backlog drains first.
	ListDueColdMoves(ctx context.Context, now time.Time, limit int) ([]LifecycleRow, error)

	// ListDueDeletes returns up to `limit` rows whose status IN ('stored',
	// 'cold') AND delete_at IS NOT NULL AND delete_at <= now. Rows with
	// delete_at IS NULL (legal hold) are skipped. Ordered by delete_at
	// ascending.
	ListDueDeletes(ctx context.Context, now time.Time, limit int) ([]LifecycleRow, error)

	// MarkCold transitions a row from status='stored' to status='cold'.
	// Returns 1 if the CAS hit (status was 'stored'), 0 if the row had
	// already moved on (stale read — caller skips). Errors are reserved
	// for genuine SQL failures.
	MarkCold(ctx context.Context, id uuid.UUID) (int64, error)

	// MarkDeleted transitions a row from status IN ('stored','cold') to
	// status='deleted'. Returns 1 on success, 0 if the row was already
	// deleted concurrently.
	MarkDeleted(ctx context.Context, id uuid.UUID) (int64, error)

	// SampleForVerify returns up to `limit` rows from the eligible set
	// (status IN ('stored','cold') AND (verified_at IS NULL OR
	// verified_at < now() - interval '7 days')) using TABLESAMPLE
	// BERNOULLI(percent) for a single-pass O(n) skip-filter. samplePct
	// is clamped to (0, 100]; pass 100 to exhaust the eligible set.
	//
	// Status='deleted' rows are excluded — there is no S3 object to verify.
	// Recently-verified rows (last 7 days) are excluded so load spreads
	// evenly across the verify window.
	SampleForVerify(ctx context.Context, samplePct float64, limit int) ([]LifecycleRow, error)

	// UpdateVerifyResult writes verified_at=now() and integrity_ok=ok for
	// the given recording id. Idempotent: re-applying with the same
	// (id, ok) is a benign overwrite of verified_at.
	UpdateVerifyResult(ctx context.Context, id uuid.UUID, ok bool) error
}

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sociopulse/platform/pkg/postgres"
)

// ErrCallNotFound is returned by InsertRecordingIdempotent and GetByCallID
// when the request references a call that does not exist for the given
// tenant.
var ErrCallNotFound = errors.New("recording.store: call not found")

// PostgresStore is the canonical RecordingStore implementation. It holds no
// per-instance state — a single pointer can be shared across goroutines.
type PostgresStore struct {
	pool *postgres.Pool
}

// NewPostgresStore constructs a store backed by pool. The store does NOT
// take ownership of the pool — callers manage its lifecycle.
func NewPostgresStore(pool *postgres.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// insertRecordingSQL is a single statement that:
//  1. Tries to INSERT the row (ON CONFLICT (call_id) DO NOTHING).
//  2. If the conflict fired, returns the existing row's id + committed_at.
//  3. Reports replay=true iff the row already existed.
//
// Single round-trip for both fresh and replay paths, with no race window
// between conflict probe and read-back.
const insertRecordingSQL = `
WITH ins AS (
    INSERT INTO call_recordings (
        id, call_id, tenant_id, s3_bucket, audio_object_key, dek_object_key,
        kms_key_id, encrypted_dek, bytes_size, duration_ms, sha256_hex,
        codec, sample_rate, status, committed_at, delete_at, cold_at,
        recorded_at, ingest_agent_id
    )
    VALUES (
        $1, $2, $3, $4, $5, $6,
        $7, $8, $9, $10, $11,
        $12, $13, $14, $15, $16, $17,
        $18, $19
    )
    ON CONFLICT (call_id) DO NOTHING
    RETURNING id, committed_at
)
SELECT
    COALESCE((SELECT id           FROM ins),
             (SELECT id           FROM call_recordings WHERE call_id = $2)) AS id,
    COALESCE((SELECT committed_at FROM ins),
             (SELECT committed_at FROM call_recordings WHERE call_id = $2)) AS committed_at,
    NOT EXISTS (SELECT 1 FROM ins) AS replay
`

// InsertRecordingIdempotent persists r inside the caller's transaction.
// Idempotent on call_id: a duplicate Commit returns the existing row's full
// data with replay=true; r.S3Bucket / r.AudioObjectKey / etc. are NOT
// overwritten on replay.
//
// FK violation on call_id (no parent in calls(id, tenant_id)) returns
// ErrCallNotFound. The check is explicit (SELECT EXISTS) ahead of the INSERT
// so the caller's TX is rolled back cleanly without surfacing a pgconn error.
func (s *PostgresStore) InsertRecordingIdempotent(ctx context.Context, tx postgres.Tx, r RecordingRow) (RecordingRow, bool, error) {
	// 1. FK check — call must exist in same tenant. Explicit so the error
	//    code is stable across pg versions and the message is recognisable.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM calls WHERE id = $1 AND tenant_id = $2)`,
		r.CallID, r.TenantID,
	).Scan(&exists); err != nil {
		return RecordingRow{}, false, fmt.Errorf("recording.store: call exists check: %w", err)
	}
	if !exists {
		return RecordingRow{}, false, ErrCallNotFound
	}

	// 2. Atomic INSERT-or-return-existing.
	var (
		id          uuid.UUID
		committedAt time.Time
		replay      bool
	)
	if err := tx.QueryRow(ctx, insertRecordingSQL,
		r.ID, r.CallID, r.TenantID, r.S3Bucket, r.AudioObjectKey, r.DEKObjectKey,
		r.KMSKeyID, r.EncryptedDEK, r.BytesSize, r.DurationMS, r.SHA256Hex,
		r.Codec, r.SampleRate, r.Status, r.CommittedAt, r.DeleteAt, r.ColdAt,
		r.RecordedAt, r.IngestAgentID,
		// $16 r.DeleteAt is *time.Time — pgx sends NULL for nil, value for non-nil.
	).Scan(&id, &committedAt, &replay); err != nil {
		// Defence-in-depth: if a different goroutine deleted the parent
		// `calls` row between our exists-check and the INSERT, we'd hit
		// FK 23503 here. Map it to ErrCallNotFound for symmetry.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return RecordingRow{}, false, ErrCallNotFound
		}
		return RecordingRow{}, false, fmt.Errorf("recording.store: insert call_recordings: %w", err)
	}

	// 3. On fresh insert: patch the returned row with DB-assigned id and
	//    committed_at (the CTE only returns those two). The rest of the fields
	//    are as submitted.
	//    On replay: re-read the full existing row so callers get the original
	//    stored data, not the caller's potentially-different input fields.
	if !replay {
		r.ID = id
		r.CommittedAt = committedAt
		return r, false, nil
	}

	const readSQL = `
SELECT id, call_id, tenant_id, s3_bucket, audio_object_key, dek_object_key,
       kms_key_id, encrypted_dek, bytes_size, duration_ms, sha256_hex,
       codec, sample_rate, status, committed_at, delete_at, cold_at,
       recorded_at, verified_at, integrity_ok, ingest_agent_id
FROM call_recordings
WHERE id = $1
`
	var existing RecordingRow
	if err := tx.QueryRow(ctx, readSQL, id).Scan(
		&existing.ID, &existing.CallID, &existing.TenantID,
		&existing.S3Bucket, &existing.AudioObjectKey, &existing.DEKObjectKey,
		&existing.KMSKeyID, &existing.EncryptedDEK, &existing.BytesSize,
		&existing.DurationMS, &existing.SHA256Hex,
		&existing.Codec, &existing.SampleRate, &existing.Status,
		&existing.CommittedAt, &existing.DeleteAt, &existing.ColdAt,
		&existing.RecordedAt, &existing.VerifiedAt, &existing.IntegrityOK,
		&existing.IngestAgentID,
	); err != nil {
		return RecordingRow{}, false, fmt.Errorf("recording.store: read existing on replay: %w", err)
	}
	return existing, true, nil
}

// GetByCallID returns the recording for (tenantID, callID). Returns
// ErrCallNotFound on a miss.
//
// The query runs inside a WithTenant transaction so that the RLS policy on
// call_recordings resolves correctly. The tenant_id predicate in the WHERE
// clause is defence-in-depth above the RLS filter.
func (s *PostgresStore) GetByCallID(ctx context.Context, tenantID, callID uuid.UUID) (RecordingRow, error) {
	const q = `
SELECT id, call_id, tenant_id, s3_bucket, audio_object_key, dek_object_key,
       kms_key_id, encrypted_dek, bytes_size, duration_ms, sha256_hex,
       codec, sample_rate, status, committed_at, delete_at, cold_at,
       recorded_at, verified_at, integrity_ok, ingest_agent_id
FROM call_recordings
WHERE tenant_id = $1 AND call_id = $2
`
	var r RecordingRow
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		scanErr := tx.QueryRow(ctx, q, tenantID, callID).Scan(
			&r.ID, &r.CallID, &r.TenantID, &r.S3Bucket, &r.AudioObjectKey, &r.DEKObjectKey,
			&r.KMSKeyID, &r.EncryptedDEK, &r.BytesSize, &r.DurationMS, &r.SHA256Hex,
			&r.Codec, &r.SampleRate, &r.Status, &r.CommittedAt, &r.DeleteAt, &r.ColdAt,
			&r.RecordedAt, &r.VerifiedAt, &r.IntegrityOK, &r.IngestAgentID,
		)
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return RecordingRow{}, ErrCallNotFound
	}
	if err != nil {
		return RecordingRow{}, fmt.Errorf("recording.store: get by call_id: %w", err)
	}
	return r, nil
}

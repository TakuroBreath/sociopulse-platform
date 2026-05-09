// Package store is the persistence layer for the recording module. It owns
// SQL access to the call_recordings table and is the only package that
// touches that schema.
package store

import (
	"time"

	"github.com/google/uuid"
)

// RecordingRow mirrors the call_recordings table 1:1 (column order matches
// migrations/000010_recording_evolve.up.sql). Constructed by the service
// layer; owned by no one outside the recording module.
//
// Nullable columns are pointers to keep zero-value semantics aligned with
// SQL NULL: RecordingRow.DEKObjectKey == nil ↔ dek_object_key IS NULL.
type RecordingRow struct {
	ID             uuid.UUID
	CallID         uuid.UUID
	TenantID       uuid.UUID
	S3Bucket       string
	AudioObjectKey string
	DEKObjectKey   *string
	KMSKeyID       string
	EncryptedDEK   []byte
	BytesSize      int64
	DurationMS     int64
	SHA256Hex      string
	Codec          string
	SampleRate     int32
	Status         string
	CommittedAt    time.Time
	// DeleteAt is nullable in the schema (NULL = "no scheduled deletion") so a
	// pointer type lets pgx round-trip NULL via a nil. Plan 12.1 callers always
	// supply a non-nil value (Commit validation rejects zero), but the column
	// can carry NULL for legal-hold/eternal-retention scenarios introduced
	// later.
	DeleteAt      *time.Time
	ColdAt        time.Time
	RecordedAt    time.Time
	VerifiedAt    *time.Time
	IntegrityOK   *bool
	IngestAgentID string
}

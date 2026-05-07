// Package api defines public contracts for the recording module.
// Other modules import only from this package — never from recording/service or recording/store.
//
// recording owns: recording metadata CRUD (call_recordings table),
// gRPC RecordingService.Commit called by cmd/recording-uploader, streamed
// S3 reads with envelope decryption (DEK from tenancy), integrity
// verification (sha256 redo), search with cursor pagination, retention
// transitions (hot → cold after 365 d, delete after +730 d total).
package api

import (
	"io"
	"time"

	"github.com/google/uuid"
)

// CommitInput is the gRPC input passed by cmd/recording-uploader after
// the audio file has been uploaded to S3. The service inserts a row into
// call_recordings and emits the recording.uploaded event.
type CommitInput struct {
	TenantID       uuid.UUID
	CallID         uuid.UUID
	S3Bucket       string
	AudioObjectKey string
	DEKObjectKey   string
	KMSKeyID       string
	EncryptedDEK   []byte
	BytesSize      int64
	Duration       time.Duration
	SHA256Hex      string // 64 hex chars
	Codec          string // "opus"
	SampleRate     int32
	DeleteAt       time.Time
	ColdAt         time.Time
	IngestAgentID  string
	RecordedAt     time.Time
}

// CommitOutput is the gRPC return of RecordingService.Commit.
// IdempotentReplay=true means the same (CallID, SHA256Hex) was already committed.
type CommitOutput struct {
	RecordingID      uuid.UUID
	CallID           uuid.UUID
	CommittedAt      time.Time
	IdempotentReplay bool
}

// RecordingMetadata is the public projection of a call_recordings row.
// It does not include the DEK material — that lives only in S3 sidecar objects.
type RecordingMetadata struct {
	RecordingID    uuid.UUID
	CallID         uuid.UUID
	TenantID       uuid.UUID
	S3Bucket       string
	AudioObjectKey string
	BytesSize      int64
	Duration       time.Duration
	SHA256Hex      string
	Status         string // "stored" | "cold" | "deleted"
	CommittedAt    time.Time
	DeleteAt       time.Time
	ColdAt         time.Time
	VerifiedAt     *time.Time
}

// SearchQuery narrows RecordingService.Search.
type SearchQuery struct {
	ProjectID  *uuid.UUID
	OperatorID *uuid.UUID
	Status     []string
	From       *time.Time
	To         *time.Time
	Cursor     string // opaque, encoded committed_at + recording_id
	Limit      int    // 1..200, default 50
}

// SearchResult is the page-with-cursor response for RecordingService.Search.
type SearchResult struct {
	Items      []RecordingMetadata
	NextCursor string
	HasMore    bool
}

// ByteRange is a half-open range request for OpenAudioStream.
// End=-1 means open-ended (read to EOF).
type ByteRange struct {
	Start int64
	End   int64 // inclusive; -1 means open-ended
}

// AudioStream is the streamed-decrypt return of OpenAudioStream.
// Caller must Close the Reader to release the underlying S3 connection.
type AudioStream struct {
	Reader        io.ReadCloser
	ContentType   string
	ContentLength int64 // total decrypted length, regardless of Range
	StartOffset   int64
	EndOffset     int64
}

// VerifyResult is the integrity-check return of VerifyChecksum.
type VerifyResult struct {
	OK           bool
	ExpectedSHA  string
	ActualSHA    string
	BytesScanned int64
	DurationMS   int64
}

// RetentionStats summarises one RetentionPlanner.RunPass invocation.
type RetentionStats struct {
	ScannedRows int64
	MovedToCold int64
	Deleted     int64
	Errors      int64
}

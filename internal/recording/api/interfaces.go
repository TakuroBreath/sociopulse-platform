package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RecordingService is the public surface for recording metadata and audio access.
// The gRPC server (called by cmd/recording-uploader) hits Commit; the HTTP
// server hits Get / Search / OpenAudioStream / VerifyChecksum.
type RecordingService interface {
	// Commit registers a freshly uploaded recording. Idempotent on (CallID, SHA256Hex).
	Commit(ctx context.Context, in CommitInput) (CommitOutput, error)
	// Get returns the metadata for the recording belonging to (tenantID, callID).
	Get(ctx context.Context, tenantID, callID uuid.UUID) (RecordingMetadata, error)
	// Search returns one page of recordings matching q.
	Search(ctx context.Context, tenantID uuid.UUID, q SearchQuery) (SearchResult, error)
	// OpenAudioStream returns a streamed, decrypted reader for the audio.
	// byteRange may be nil (return the full object); when set, the returned
	// stream covers exactly the requested range.
	OpenAudioStream(ctx context.Context, tenantID, callID uuid.UUID, byteRange *ByteRange) (AudioStream, error)
	// VerifyChecksum reads the entire object back and recomputes its sha256.
	VerifyChecksum(ctx context.Context, tenantID, callID uuid.UUID) (VerifyResult, error)
}

// URLSigner mints time-limited presigned URLs for direct browser playback.
type URLSigner interface {
	// Sign returns a presigned URL valid for ttl seconds. Each call is
	// audited as recording.accessed.
	Sign(ctx context.Context, tenantID, callID uuid.UUID, ttl time.Duration) (signedURL string, err error)
}

// RetentionPlanner runs the per-tenant retention pass.
type RetentionPlanner interface {
	// RunPass scans recordings whose status transitions are due and applies them.
	// Idempotent — safe to re-run after partial failure.
	RunPass(ctx context.Context, now time.Time) (RetentionStats, error)
}

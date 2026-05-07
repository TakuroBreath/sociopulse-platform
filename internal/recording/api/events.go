package api

import (
	"fmt"

	"github.com/google/uuid"
)

// NATS subject placeholders for the durable JetStream stream RECORDING
// (30-day retention, via outbox).
const (
	// SubjectRecordingUploaded is published after a successful Commit.
	SubjectRecordingUploaded = "tenant.<t>.recording.uploaded"
)

// SubjectRecordingUploadedFor returns the concrete subject for the
// recording.uploaded event for the given tenant.
func SubjectRecordingUploadedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.recording.uploaded", tenantID)
}

// Audit action constants. Recording mirrors two state-changing actions to
// the audit module via the canonical tenant.<t>.audit.event subject.
const (
	// AuditActionCommitted is the audit Action set on a successful Commit.
	AuditActionCommitted = "recording.committed"
	// AuditActionAccessed is the audit Action set on every read (Sign / OpenAudioStream).
	AuditActionAccessed = "recording.accessed"
)

// asynq task type constants.
const (
	// TaskRetentionPass runs the daily retention scheduler at 03:00 МСК.
	TaskRetentionPass = "recording:retention.pass"
)

// RecordingUploadedEvent is the payload for SubjectRecordingUploaded.
// Mirrors RecordingMetadata but omits S3 paths so subscribers cannot
// short-circuit the audited read path.
type RecordingUploadedEvent struct {
	RecordingID uuid.UUID `json:"recording_id"`
	CallID      uuid.UUID `json:"call_id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	BytesSize   int64     `json:"bytes_size"`
	DurationMS  int64     `json:"duration_ms"`
	SHA256Hex   string    `json:"sha256"`
	Status      string    `json:"status"`
	CommittedAt int64     `json:"committed_at"` // unix seconds
}

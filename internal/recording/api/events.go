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
	// SubjectRecordingCallDeleted is published after the retention worker
	// hard-deletes a recording (S3 object purged, status flipped to
	// 'deleted'). The "<t>" placeholder is for documentation only — code
	// must use SubjectRecordingCallDeletedFor to render the concrete
	// subject for a tenant.
	SubjectRecordingCallDeleted = "tenant.<t>.recording.call.deleted"
)

// SubjectRecordingUploadedFor returns the concrete subject for the
// recording.uploaded event for the given tenant.
func SubjectRecordingUploadedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.recording.uploaded", tenantID)
}

// SubjectRecordingCallDeletedFor returns the concrete subject for the
// recording.call.deleted event for the given tenant. The retention
// worker uses this when it appends the outbox row that signals downstream
// (audit module, BI exports) that the recording's audio object has been
// permanently purged.
func SubjectRecordingCallDeletedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.recording.call.deleted", tenantID)
}

// Audit action constants. Recording mirrors three state-changing actions
// to the audit module via the canonical tenant.<t>.audit.event subject.
const (
	// AuditActionCommitted is the audit Action set on a successful Commit.
	AuditActionCommitted = "recording.committed"
	// AuditActionAccessed is the audit Action set on every read (Sign / OpenAudioStream).
	AuditActionAccessed = "recording.accessed"
	// AuditActionColdMoved is the audit Action emitted by the retention
	// worker when a row transitions from status='stored' to 'cold'.
	AuditActionColdMoved = "recording.cold_moved"
	// AuditActionDeleted is the audit Action emitted by the retention worker
	// after Phase A (S3 object delete) succeeds and Phase B (DB status flip
	// + outbox event) commits.
	AuditActionDeleted = "recording.deleted"
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

// RecordingCallDeletedEvent is the payload published on
// SubjectRecordingCallDeletedFor(tenantID) after the retention worker
// hard-deletes a recording. Subscribers (audit, BI) MUST treat the row
// as gone — both the S3 audio object and the DB status are now 'deleted'.
//
// Reason is "retention" for worker-driven deletes (the canonical path).
// A future admin-driven manual-delete path may emit "manual" — workers
// MUST NOT use that value.
type RecordingCallDeletedEvent struct {
	RecordingID uuid.UUID `json:"recording_id"`
	CallID      uuid.UUID `json:"call_id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	DeletedAt   int64     `json:"deleted_at"` // unix seconds
	Reason      string    `json:"reason"`     // "retention" | "manual"
}

package api

import (
	"fmt"

	"github.com/google/uuid"
)

// NATS subject placeholders. The reports module publishes a "ready" event
// when an async job finishes and (separately) a per-user notification.
const (
	// SubjectReportReady is published after an async job succeeds.
	// Payload includes the presigned download URL.
	SubjectReportReady = "tenant.<t>.reports.report.ready"
)

// SubjectReportReadyFor returns the concrete subject for the
// reports.report.ready event for the given tenant.
func SubjectReportReadyFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.reports.report.ready", tenantID)
}

// SubjectUserNotifyFor returns the concrete subject for the per-user
// notification a job's completion produces. This subject is owned by
// realtime; reports just publishes to it. Helper kept here for clarity
// at the call site.
func SubjectUserNotifyFor(tenantID, userID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.notify.user.%s", tenantID, userID)
}

// Audit action constants. Reports mirrors every render or download to the
// audit module via the canonical tenant.<t>.audit.event subject.
const (
	// AuditActionExport is the audit Action set on every Render or Download.
	AuditActionExport = "reports.export"
)

// asynq task type constants.
const (
	// TaskJobRun is the single asynq task type — payload carries JobInput.
	// Worker is internal/reports/service.JobConsumer.
	TaskJobRun = "reports:job.run"
)

// ReportReadyEvent is the payload for SubjectReportReady.
type ReportReadyEvent struct {
	JobID       string `json:"job_id"`
	TenantID    string `json:"tenant_id"`
	Kind        string `json:"kind"`
	Format      string `json:"format"`
	Filename    string `json:"filename"`
	BytesSize   int64  `json:"bytes_size"`
	DownloadURL string `json:"download_url"` // presigned, 24h TTL
}

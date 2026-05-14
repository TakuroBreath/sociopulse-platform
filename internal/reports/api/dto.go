// Package api defines public contracts for the reports module.
// Other modules import only from this package — never from reports/service or reports/store.
//
// reports owns six preset report templates (operator efficiency, project
// summary, calls by status, finance, quality control, hourly activity),
// custom reports parameterised by period+project+format, async generation
// via asynq for large windows (> 30 d or > 100 k rows), XLSX/CSV/PDF
// renderers, presigned download URLs (24 h TTL), and audit on export.
// Reports build on analytics MetricsQuery + recording metadata.
package api

import (
	"time"

	"github.com/google/uuid"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
)

// ReportKind enumerates the preset reports plus a "custom" sentinel.
type ReportKind string

const (
	KindOperatorEfficiency ReportKind = "operator_efficiency"
	KindProjectSummary     ReportKind = "project_summary"
	KindCallsByStatus      ReportKind = "calls_by_status"
	KindFinance            ReportKind = "finance"
	KindQualityControl     ReportKind = "quality_control"
	KindHourlyActivity     ReportKind = "hourly_activity"
	KindCustom             ReportKind = "custom"
)

// ExportFormat enumerates the renderer output formats.
type ExportFormat string

const (
	FormatXLSX ExportFormat = "xlsx"
	FormatCSV  ExportFormat = "csv"
	FormatPDF  ExportFormat = "pdf"
)

// Window is the time range every report takes; aliased from analytics for
// clarity in this package so callers do not have to import both packages.
type Window = analyticsapi.Window

// RenderInput is the canonical input for both ReportRenderer.Render and
// ReportRunner.Run (they share the same shape).
type RenderInput struct {
	Kind     ReportKind
	Format   ExportFormat
	Params   map[string]any // kind-specific (project_id, operator_id, ...)
	Window   Window
	TenantID uuid.UUID
	ActorID  uuid.UUID
}

// RenderResult is the return of ReportRenderer.Render.
type RenderResult struct {
	Bytes    []byte
	Filename string
	MIME     string
	SHA256   string
}

// RunInput is an alias for RenderInput. Kept as a distinct type name so
// the runner's signature reads naturally.
type RunInput = RenderInput

// RunResult is an alias for RenderResult.
type RunResult = RenderResult

// JobInput extends RenderInput with the user that should be notified on completion.
type JobInput struct {
	RenderInput
	NotifyUserID uuid.UUID
}

// JobTicket is the synchronous receipt for an enqueued async job.
type JobTicket struct {
	JobID    string
	QueuedAt time.Time
}

// JobState enumerates the lifecycle states of a Job.
type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCanceled  JobState = "canceled"
)

// Job is the public projection of a report-job row.
type Job struct {
	ID          string
	TenantID    uuid.UUID
	Kind        ReportKind
	Format      ExportFormat
	Params      map[string]any
	Window      Window
	State       JobState
	StartedAt   *time.Time
	FinishedAt  *time.Time
	BytesSize   int64
	Filename    string
	DownloadURL string // populated when State=succeeded; presigned, 24h TTL
	Error       string
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
}

// ListJobsFilter narrows JobQueue.List.
//
// TenantID is REQUIRED — the JobQueue.List interface contract is per-
// tenant scoped, but the method signature does not carry tenantID as a
// dedicated parameter. The HTTP handler (Task 7) reads claims.TenantID
// from the gin context and injects it here before delegating to the
// Queue. A zero TenantID is rejected by Queue.List with ErrInvalidParams.
type ListJobsFilter struct {
	TenantID uuid.UUID
	State    *JobState
	Kind     *ReportKind
	From     *time.Time
	To       *time.Time
	Cursor   string
	Limit    int
}

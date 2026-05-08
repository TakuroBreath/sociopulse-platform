package api

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// NATS subject constants. The crm module publishes these to inform other
// modules (analytics, dialer, realtime) of project / respondent / quota /
// DNC changes. Subjects follow the canonical scheme tenant.<t>.<area>...
//
// The subject constants below contain literal "<t>" placeholders; the
// runtime materialises concrete subjects via the Subject<X>For helpers.
const (
	// SubjectProjectCreated is published when a project row is inserted.
	SubjectProjectCreated = "tenant.<t>.crm.project.created"
	// SubjectProjectUpdated is published on UpdateProjectInput application.
	SubjectProjectUpdated = "tenant.<t>.crm.project.updated"
	// SubjectProjectStatus is published on Pause/Resume/Archive.
	SubjectProjectStatus = "tenant.<t>.crm.project.status_changed"
	// SubjectImportStarted is published when an import job begins running.
	SubjectImportStarted = "tenant.<t>.crm.respondents.import.started"
	// SubjectImportProgress is published every batch (~1s) while an import runs.
	SubjectImportProgress = "tenant.<t>.crm.respondents.import.progress"
	// SubjectImportFinished is published on import success.
	SubjectImportFinished = "tenant.<t>.crm.respondents.import.finished"
	// SubjectImportFailed is published when an import job fails terminally.
	SubjectImportFailed = "tenant.<t>.crm.respondents.import.failed"
	// SubjectRespondentDelete is published on a 152-ФЗ deletion request.
	SubjectRespondentDelete = "tenant.<t>.crm.respondent.deletion_requested"
	// SubjectQuotaIncrement is published when a quota cell counter increases.
	SubjectQuotaIncrement = "tenant.<t>.crm.quota.incremented"
	// SubjectDNCAdded is published when a DNC entry is added.
	SubjectDNCAdded = "tenant.<t>.crm.dnc.added"
)

// SubjectProjectCreatedFor returns the concrete subject for the
// crm.project.created event for the given tenant.
func SubjectProjectCreatedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.project.created", tenantID)
}

// SubjectProjectUpdatedFor returns the concrete subject for the
// crm.project.updated event for the given tenant.
func SubjectProjectUpdatedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.project.updated", tenantID)
}

// SubjectProjectStatusFor returns the concrete subject for the
// crm.project.status_changed event for the given tenant.
func SubjectProjectStatusFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.project.status_changed", tenantID)
}

// SubjectImportStartedFor returns the concrete subject for the
// crm.respondents.import.started event for the given tenant.
func SubjectImportStartedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.respondents.import.started", tenantID)
}

// SubjectImportProgressFor returns the concrete subject for the
// crm.respondents.import.progress event for the given tenant.
func SubjectImportProgressFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.respondents.import.progress", tenantID)
}

// SubjectImportFinishedFor returns the concrete subject for the
// crm.respondents.import.finished event for the given tenant.
func SubjectImportFinishedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.respondents.import.finished", tenantID)
}

// SubjectImportFailedFor returns the concrete subject for the
// crm.respondents.import.failed event for the given tenant.
func SubjectImportFailedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.respondents.import.failed", tenantID)
}

// SubjectRespondentDeleteFor returns the concrete subject for the
// crm.respondent.deletion_requested event for the given tenant.
func SubjectRespondentDeleteFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.respondent.deletion_requested", tenantID)
}

// SubjectQuotaIncrementFor returns the concrete subject for the
// crm.quota.incremented event for the given tenant.
func SubjectQuotaIncrementFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.quota.incremented", tenantID)
}

// SubjectDNCAddedFor returns the concrete subject for the crm.dnc.added
// event for the given tenant.
func SubjectDNCAddedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.crm.dnc.added", tenantID)
}

// asynq task type constants. The crm module enqueues these via cmd/api;
// cmd/worker registers handlers.
const (
	// TaskRespondentImport drives the async CSV/XLSX respondent import.
	TaskRespondentImport = "crm:respondent.import"
	// TaskRespondentsPurge runs the daily 30-day soft-deleted respondent purge.
	TaskRespondentsPurge = "crm:respondents.purge"
	// TaskQuotasRecompute reconciles the Redis quota counters with Postgres truth.
	TaskQuotasRecompute = "crm:quotas.recompute"
	// TaskDNCImport drives the async DNC list bulk import.
	TaskDNCImport = "crm:dnc.import"
)

// ProjectCreatedEvent is the payload for SubjectProjectCreated.
// Mirrors the row inserted, minus columns the frontend doesn't need
// at notification-time (timestamps the consumer can lazily fetch).
type ProjectCreatedEvent struct {
	ProjectID   uuid.UUID `json:"project_id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Customer    string    `json:"customer,omitempty"`
	TargetCount int       `json:"target_count"`
	CreatedAt   time.Time `json:"created_at"`
}

// ProjectUpdatedEvent is the payload for SubjectProjectUpdated. Only
// the field-names that changed are included — consumers re-fetch the
// row if they need full state. This keeps the bus traffic small.
type ProjectUpdatedEvent struct {
	ProjectID uuid.UUID `json:"project_id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	Changed   []string  `json:"changed"` // e.g. ["name", "target_count"]
	UpdatedAt time.Time `json:"updated_at"`
}

// ProjectStatusChangedEvent is the payload for SubjectProjectStatus.
type ProjectStatusChangedEvent struct {
	ProjectID  uuid.UUID     `json:"project_id"`
	TenantID   uuid.UUID     `json:"tenant_id"`
	OldStatus  ProjectStatus `json:"old_status"`
	NewStatus  ProjectStatus `json:"new_status"`
	ChangedAt  time.Time     `json:"changed_at"`
	ArchivedAt *time.Time    `json:"archived_at,omitempty"`
}

// ImportProgressEvent is the payload for SubjectImportProgress.
type ImportProgressEvent struct {
	JobID     string `json:"job_id"`
	Total     int    `json:"total"`
	Processed int    `json:"processed"`
	Inserted  int    `json:"inserted"`
	Skipped   int    `json:"skipped"`
}

// ImportFinishedEvent is the payload for SubjectImportFinished.
type ImportFinishedEvent struct {
	JobID    string `json:"job_id"`
	Total    int    `json:"total"`
	Inserted int    `json:"inserted"`
	Skipped  int    `json:"skipped"`
}

// ImportFailedEvent is the payload for SubjectImportFailed.
type ImportFailedEvent struct {
	JobID string `json:"job_id"`
	Error string `json:"error"`
}

// RespondentDeletionRequestedEvent is the payload for SubjectRespondentDelete.
type RespondentDeletionRequestedEvent struct {
	RespondentID uuid.UUID `json:"respondent_id"`
	DeleteAt     int64     `json:"delete_at"` // unix seconds
}

// QuotaIncrementedEvent is the payload for SubjectQuotaIncrement.
type QuotaIncrementedEvent struct {
	ProjectID      uuid.UUID `json:"project_id"`
	DimensionKind  string    `json:"dimension_kind"`
	DimensionValue string    `json:"dimension_value"`
	Done           int       `json:"done"`
	Target         int       `json:"target"`
}

// DNCAddedEvent is the payload for SubjectDNCAdded.
type DNCAddedEvent struct {
	ProjectID *uuid.UUID `json:"project_id,omitempty"`
	PhoneHash []byte     `json:"phone_hash"`
	Source    string     `json:"source"`
}

// Package api defines public contracts for the crm module.
// Other modules import only from this package — never from crm/service or crm/store.
//
// crm owns project lifecycle, respondents, quotas, DNC, async CSV/XLSX import,
// the 152-ФЗ deletion right (30-day soft-delete + worker purge), Russian phone
// validation/normalization (E.164 + АВС/DEF prefix mapping), and the
// real-time quota tracker with Redis cache and Postgres reconciliation worker.
package api

import (
	"time"

	"github.com/google/uuid"
)

// ProjectStatus enumerates the lifecycle states of a Project.
type ProjectStatus string

const (
	StatusActive   ProjectStatus = "active"
	StatusPaused   ProjectStatus = "paused"
	StatusArchived ProjectStatus = "archived"
)

// Project is the public projection of a project row.
type Project struct {
	ID                     uuid.UUID
	TenantID               uuid.UUID
	Code                   string
	Name                   string
	Customer               string
	Status                 ProjectStatus
	TargetCount            int
	PeriodFrom             *time.Time
	PeriodTo               *time.Time
	SurveyID               *uuid.UUID
	DefaultSurveyVersionID *uuid.UUID
	IsAdvertising          bool
	CreatedBy              *uuid.UUID
	CreatedAt              time.Time
	UpdatedAt              time.Time
	ArchivedAt             *time.Time
	Quotas                 []Quota
	Assignments            []ProjectMember
}

// CreateProjectInput is the payload for ProjectService.Create.
type CreateProjectInput struct {
	TenantID       uuid.UUID
	Code           string
	Name           string
	Customer       string
	TargetCount    int
	PeriodFrom     *time.Time
	PeriodTo       *time.Time
	SurveyID       *uuid.UUID
	IsAdvertising  bool
	InitialQuotas  []Quota
	InitialMembers []uuid.UUID
}

// UpdateProjectInput carries the patch fields for ProjectService.Update.
// Pointer-typed fields denote optional patches: nil means "leave unchanged".
type UpdateProjectInput struct {
	Name        *string
	Customer    *string
	TargetCount *int
	PeriodFrom  *time.Time
	PeriodTo    *time.Time
	SurveyID    *uuid.UUID
}

// ProjectMember describes one operator assignment to a project.
type ProjectMember struct {
	OperatorID uuid.UUID
	AssignedAt time.Time
}

// ListProjectsFilter narrows ProjectService.List.
//
// Limit/Offset are the canonical pagination knobs the service layer clamps
// to [1, 500] / >=0 (defaults: Limit=50, Offset=0). IncludeArchived defaults
// to false so admin lists hide soft-deleted rows; passing true surfaces them
// for a "show all" view. Status/Search remain optional narrow filters.
type ListProjectsFilter struct {
	TenantID        uuid.UUID
	Status          *ProjectStatus
	Search          string
	IncludeArchived bool
	Limit           int
	Offset          int
}

// ListProjectsResult is the page-with-total response for ProjectService.List.
type ListProjectsResult struct {
	Items      []Project
	TotalCount int64
}

// ProjectProgress is the live counter snapshot used by the dashboard
// "project progress" widget.
type ProjectProgress struct {
	ProjectID       uuid.UUID
	TargetCount     int
	CompletedCount  int
	InProgressCount int
	PendingCount    int
	DNCCount        int
	ExhaustedCount  int
	WrongCount      int
	PercentDone     float64
	PaceLast24h     int
	ETACompletion   *time.Time
	QuotaProgress   []QuotaSnapshot
}

// Quota is one row in a project's quota plan.
type Quota struct {
	DimensionKind  string // "region" | "gender" | "age_bucket" | "custom"
	DimensionValue string
	Target         int
}

// QuotaSnapshot is the realtime tracker's view of one quota cell.
type QuotaSnapshot struct {
	DimensionKind  string
	DimensionValue string
	Target         int
	Done           int
	PercentDone    float64
	IsFull         bool
}

// RespondentStatus enumerates the lifecycle states of a Respondent.
type RespondentStatus string

const (
	RespPending           RespondentStatus = "pending"
	RespDialing           RespondentStatus = "dialing"
	RespCompleted         RespondentStatus = "completed"
	RespDNC               RespondentStatus = "dnc"
	RespExhausted         RespondentStatus = "exhausted"
	RespWrong             RespondentStatus = "wrong"
	RespDeletionRequested RespondentStatus = "deletion-requested"
)

// Respondent is the public projection of a respondent row.
// Phone is populated only by GetWithPhone (admin-only); PhoneMasked is the
// display-safe variant returned to operators (e.g. "+7-9** ***-**-12").
type Respondent struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	ProjectID     uuid.UUID
	PhoneMasked   string
	Phone         string
	RegionCode    string
	Attributes    map[string]any
	Status        RespondentStatus
	Attempts      int
	LastAttemptAt *time.Time
	NextAttemptAt *time.Time
	Source        string // "imported" | "rdd"
	CreatedAt     time.Time
	DeleteAt      *time.Time
}

// CreateRespondentInput is the payload for RespondentService.Create.
type CreateRespondentInput struct {
	ProjectID  uuid.UUID
	Phone      string
	RegionCode string
	Attributes map[string]any
}

// SearchRespondentsFilter narrows RespondentService.Search.
type SearchRespondentsFilter struct {
	ProjectID   uuid.UUID
	Status      *RespondentStatus
	PhoneSearch string
	Region      string
	Page        int
	PageSize    int
}

// SearchRespondentsResult is the page-with-total response for RespondentService.Search.
type SearchRespondentsResult struct {
	Items      []Respondent
	TotalCount int
}

// ImportRequest is the payload for RespondentService.Import.
type ImportRequest struct {
	ProjectID    uuid.UUID
	Filename     string
	ContentType  string
	Body         []byte
	ColumnMap    map[string]string
	DefaultAttrs map[string]any
}

// ImportTicket is returned synchronously when an import is accepted; the
// real result is observed via the asynq task and the import.* events.
type ImportTicket struct {
	JobID     string
	ProjectID uuid.UUID
	Total     int
	StartedAt time.Time
}

// ImportStatus is the polling response for RespondentService.GetImportStatus.
type ImportStatus struct {
	JobID      string
	State      string // "queued" | "running" | "succeeded" | "failed"
	Total      int
	Processed  int
	Inserted   int
	Skipped    int
	Errors     []ImportError
	StartedAt  time.Time
	FinishedAt *time.Time
}

// ImportError is one row-level error from an import job.
type ImportError struct {
	Row     int
	Phone   string
	Message string
}

// DeletionRequest is the receipt returned from RespondentService.Delete.
// The respondent transitions to RespDeletionRequested and is purged after
// 30 days by the cmd/worker purge task (152-ФЗ §13.3).
type DeletionRequest struct {
	RespondentID uuid.UUID
	DeleteAt     time.Time
}

// DNCEntry is one row in the do-not-call list. PhoneMasked is the
// display-safe variant; the raw phone is never returned by the API.
type DNCEntry struct {
	PhoneMasked string
	Source      string
	AddedAt     int64
}

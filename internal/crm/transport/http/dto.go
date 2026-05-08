// Package http provides the gin HTTP transport for the crm module.
//
// Handlers are intentionally thin — they bind JSON / multipart, call
// services, and render results or errors via the helpers in
// errors.go. ALL business logic lives in internal/crm/service. The
// transport layer's only responsibility is the wire format.
//
// Routes are mounted with Mount(group, deps), which the crm module's
// composition root invokes against the gin engine carried in
// modules.Deps.HTTPRouter.
package http

import (
	"time"

	"github.com/google/uuid"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
)

// CreateProjectRequest is the body of POST /api/projects.
type CreateProjectRequest struct {
	Code        string     `json:"code" binding:"required,min=1,max=64"`
	Name        string     `json:"name" binding:"required,min=1,max=200"`
	Customer    string     `json:"customer" binding:"omitempty,max=200"`
	TargetCount int        `json:"target_count" binding:"min=0"`
	PeriodFrom  *time.Time `json:"period_from,omitempty"`
	PeriodTo    *time.Time `json:"period_to,omitempty"`
	SurveyID    *uuid.UUID `json:"survey_id,omitempty"`
}

// UpdateProjectRequest is the body of PATCH /api/projects/:id.
//
// Pointer fields signal "leave unchanged when nil" — every shape on
// the wire is JSON-omitempty + nullable so the client can patch any
// subset of fields atomically.
type UpdateProjectRequest struct {
	Name        *string    `json:"name,omitempty" binding:"omitempty,min=1,max=200"`
	Customer    *string    `json:"customer,omitempty" binding:"omitempty,max=200"`
	TargetCount *int       `json:"target_count,omitempty" binding:"omitempty,min=0"`
	PeriodFrom  *time.Time `json:"period_from,omitempty"`
	PeriodTo    *time.Time `json:"period_to,omitempty"`
	SurveyID    *uuid.UUID `json:"survey_id,omitempty"`
}

// AssignOperatorsRequest is the body of POST /api/projects/:id/assign.
type AssignOperatorsRequest struct {
	OperatorIDs []uuid.UUID `json:"operator_ids" binding:"required,min=1,dive,uuid"`
}

// CreateRespondentRequest is the body of
// POST /api/projects/:id/respondents.
type CreateRespondentRequest struct {
	Phone      string         `json:"phone" binding:"required,max=32"`
	RegionCode string         `json:"region_code,omitempty" binding:"omitempty,max=16"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Source     string         `json:"source,omitempty" binding:"omitempty,oneof=imported rdd"`
}

// ProjectDTO is the wire shape for a project. We define a stable
// transport-layer type rather than letting gin marshal api.Project
// directly so future additions to api.Project don't silently change
// the public response.
type ProjectDTO struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"tenant_id"`
	Code          string     `json:"code"`
	Name          string     `json:"name"`
	Customer      string     `json:"customer,omitempty"`
	Status        string     `json:"status"`
	TargetCount   int        `json:"target_count"`
	PeriodFrom    *time.Time `json:"period_from,omitempty"`
	PeriodTo      *time.Time `json:"period_to,omitempty"`
	SurveyID      *uuid.UUID `json:"survey_id,omitempty"`
	IsAdvertising bool       `json:"is_advertising"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ArchivedAt    *time.Time `json:"archived_at,omitempty"`
}

// ListProjectsResponse is the body of GET /api/projects.
type ListProjectsResponse struct {
	Projects   []ProjectDTO `json:"projects"`
	TotalCount int64        `json:"total_count"`
}

// ProjectProgressDTO is the wire shape for the dashboard widget.
type ProjectProgressDTO struct {
	ProjectID       string             `json:"project_id"`
	TargetCount     int                `json:"target_count"`
	CompletedCount  int                `json:"completed_count"`
	InProgressCount int                `json:"in_progress_count"`
	PendingCount    int                `json:"pending_count"`
	DNCCount        int                `json:"dnc_count"`
	ExhaustedCount  int                `json:"exhausted_count"`
	WrongCount      int                `json:"wrong_count"`
	PercentDone     float64            `json:"percent_done"`
	PaceLast24h     int                `json:"pace_last_24h"`
	ETACompletion   *time.Time         `json:"eta_completion,omitempty"`
	QuotaProgress   []QuotaSnapshotDTO `json:"quota_progress,omitempty"`
}

// QuotaSnapshotDTO is the wire shape for one quota cell snapshot.
type QuotaSnapshotDTO struct {
	DimensionKind  string  `json:"dimension_kind"`
	DimensionValue string  `json:"dimension_value"`
	Target         int     `json:"target"`
	Done           int     `json:"done"`
	PercentDone    float64 `json:"percent_done"`
	IsFull         bool    `json:"is_full"`
}

// ProjectMemberDTO is the wire shape for one assigned operator.
type ProjectMemberDTO struct {
	OperatorID string    `json:"operator_id"`
	AssignedAt time.Time `json:"assigned_at"`
	Login      string    `json:"login,omitempty"`
	FullName   string    `json:"full_name,omitempty"`
}

// ListMembersResponse is the body of GET /api/projects/:id/members.
type ListMembersResponse struct {
	Members []ProjectMemberDTO `json:"members"`
}

// RespondentDTO is the wire shape for a respondent.
//
// Phone is populated only when the caller used the explicit
// /respondents/:id/with-phone admin path; on every other read it is
// omitted (PhoneMasked is the only display surface for operators).
type RespondentDTO struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	ProjectID     string         `json:"project_id"`
	Phone         string         `json:"phone,omitempty"`
	PhoneMasked   string         `json:"phone_masked"`
	RegionCode    string         `json:"region_code,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	Status        string         `json:"status"`
	Attempts      int            `json:"attempts"`
	LastAttemptAt *time.Time     `json:"last_attempt_at,omitempty"`
	NextAttemptAt *time.Time     `json:"next_attempt_at,omitempty"`
	Source        string         `json:"source"`
	CreatedAt     time.Time      `json:"created_at"`
	DeletedAt     *time.Time     `json:"deleted_at,omitempty"`
}

// SearchRespondentsResponse is the body of
// GET /api/projects/:id/respondents.
type SearchRespondentsResponse struct {
	Respondents []RespondentDTO `json:"respondents"`
	TotalCount  int             `json:"total_count"`
	Page        int             `json:"page"`
	PageSize    int             `json:"page_size"`
}

// DeletionReceiptDTO is the body of DELETE /api/respondents/:id.
type DeletionReceiptDTO struct {
	RespondentID     string    `json:"respondent_id"`
	ScheduledPurgeAt time.Time `json:"scheduled_purge_at"`
}

// ImportTicketDTO is the body of POST /api/projects/:id/respondents/import.
type ImportTicketDTO struct {
	JobID     string    `json:"job_id"`
	ProjectID string    `json:"project_id"`
	Enqueued  bool      `json:"enqueued"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
}

// ImportStatusDTO is the body of GET /api/imports/:job_id.
type ImportStatusDTO struct {
	JobID      string           `json:"job_id"`
	State      string           `json:"state"`
	Total      int              `json:"total"`
	Processed  int              `json:"processed"`
	Inserted   int              `json:"inserted"`
	Skipped    int              `json:"skipped"`
	Errors     []ImportErrorDTO `json:"errors,omitempty"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
}

// ImportErrorDTO is one row-level error from an import job.
type ImportErrorDTO struct {
	Row     int    `json:"row"`
	Message string `json:"message"`
}

// ErrorEnvelope is the JSON shape every 4xx/5xx response uses. Mirrors
// the auth package's envelope so the wire format stays uniform across
// modules.
type ErrorEnvelope struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// projectToDTO converts an api.Project to the transport-level DTO.
func projectToDTO(p crmapi.Project) ProjectDTO {
	return ProjectDTO{
		ID:            p.ID.String(),
		TenantID:      p.TenantID.String(),
		Code:          p.Code,
		Name:          p.Name,
		Customer:      p.Customer,
		Status:        string(p.Status),
		TargetCount:   p.TargetCount,
		PeriodFrom:    p.PeriodFrom,
		PeriodTo:      p.PeriodTo,
		SurveyID:      p.SurveyID,
		IsAdvertising: p.IsAdvertising,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		ArchivedAt:    p.ArchivedAt,
	}
}

// projectsToDTO is the slice form of projectToDTO.
func projectsToDTO(in []crmapi.Project) []ProjectDTO {
	out := make([]ProjectDTO, len(in))
	for i, p := range in {
		out[i] = projectToDTO(p)
	}
	return out
}

// progressToDTO converts api.ProjectProgress to the wire shape.
func progressToDTO(p crmapi.ProjectProgress) ProjectProgressDTO {
	out := ProjectProgressDTO{
		ProjectID:       p.ProjectID.String(),
		TargetCount:     p.TargetCount,
		CompletedCount:  p.CompletedCount,
		InProgressCount: p.InProgressCount,
		PendingCount:    p.PendingCount,
		DNCCount:        p.DNCCount,
		ExhaustedCount:  p.ExhaustedCount,
		WrongCount:      p.WrongCount,
		PercentDone:     p.PercentDone,
		PaceLast24h:     p.PaceLast24h,
		ETACompletion:   p.ETACompletion,
	}
	if len(p.QuotaProgress) > 0 {
		out.QuotaProgress = make([]QuotaSnapshotDTO, len(p.QuotaProgress))
		for i, q := range p.QuotaProgress {
			out.QuotaProgress[i] = QuotaSnapshotDTO{
				DimensionKind:  q.DimensionKind,
				DimensionValue: q.DimensionValue,
				Target:         q.Target,
				Done:           q.Done,
				PercentDone:    q.PercentDone,
				IsFull:         q.IsFull,
			}
		}
	}
	return out
}

// memberToDTO converts api.ProjectMember to the wire shape.
func memberToDTO(m crmapi.ProjectMember) ProjectMemberDTO {
	return ProjectMemberDTO{
		OperatorID: m.OperatorID.String(),
		AssignedAt: m.AssignedAt,
		Login:      m.Login,
		FullName:   m.FullName,
	}
}

// membersToDTO is the slice form of memberToDTO.
func membersToDTO(in []crmapi.ProjectMember) []ProjectMemberDTO {
	out := make([]ProjectMemberDTO, len(in))
	for i, m := range in {
		out[i] = memberToDTO(m)
	}
	return out
}

// respondentToDTO converts api.Respondent to the wire shape. The
// caller decides whether to populate Phone (admin-only path); this
// helper carries whatever the service supplied.
func respondentToDTO(r crmapi.Respondent) RespondentDTO {
	return RespondentDTO{
		ID:            r.ID.String(),
		TenantID:      r.TenantID.String(),
		ProjectID:     r.ProjectID.String(),
		Phone:         r.Phone,
		PhoneMasked:   r.PhoneMasked,
		RegionCode:    r.RegionCode,
		Attributes:    r.Attributes,
		Status:        string(r.Status),
		Attempts:      r.Attempts,
		LastAttemptAt: r.LastAttemptAt,
		NextAttemptAt: r.NextAttemptAt,
		Source:        r.Source,
		CreatedAt:     r.CreatedAt,
		DeletedAt:     r.DeleteAt,
	}
}

// respondentsToDTO is the slice form of respondentToDTO.
func respondentsToDTO(in []crmapi.Respondent) []RespondentDTO {
	out := make([]RespondentDTO, len(in))
	for i, r := range in {
		out[i] = respondentToDTO(r)
	}
	return out
}

// importTicketToDTO converts api.ImportTicket to the wire shape.
func importTicketToDTO(t crmapi.ImportTicket) ImportTicketDTO {
	return ImportTicketDTO{
		JobID:     t.JobID,
		ProjectID: t.ProjectID.String(),
		Enqueued:  t.Enqueued,
		Status:    t.Status,
		StartedAt: t.StartedAt,
	}
}

// importStatusToDTO converts api.ImportStatus to the wire shape.
func importStatusToDTO(s crmapi.ImportStatus) ImportStatusDTO {
	out := ImportStatusDTO{
		JobID:      s.JobID,
		State:      s.State,
		Total:      s.Total,
		Processed:  s.Processed,
		Inserted:   s.Inserted,
		Skipped:    s.Skipped,
		StartedAt:  s.StartedAt,
		FinishedAt: s.FinishedAt,
	}
	if len(s.Errors) > 0 {
		out.Errors = make([]ImportErrorDTO, len(s.Errors))
		for i, e := range s.Errors {
			out.Errors[i] = ImportErrorDTO{Row: e.Row, Message: e.Message}
		}
	}
	return out
}

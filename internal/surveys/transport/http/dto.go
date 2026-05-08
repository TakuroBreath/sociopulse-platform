// Package http provides the gin HTTP transport for the surveys module.
//
// Handlers are intentionally thin — they bind JSON, call services or the
// runtime, and render results or errors via the helpers in errors.go.
// ALL business logic lives in internal/surveys/service and
// internal/surveys/runtime. The transport layer's only responsibility
// is the wire format.
//
// Routes are mounted with Mount(group, deps), which the surveys
// module's composition root invokes against the gin engine carried in
// modules.Deps.HTTPRouter.
package http

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	surveysapi "github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/schemavalidator"
)

// CreateSurveyRequest is the body of POST /api/surveys.
type CreateSurveyRequest struct {
	Name        string `json:"name" binding:"required,min=1,max=200"`
	Description string `json:"description" binding:"omitempty,max=4096"`
	PrimaryMode string `json:"primary_mode" binding:"omitempty,oneof=form flow"`
}

// UpdateSurveyRequest is the body of PATCH /api/surveys/:id.
//
// Pointer fields signal "leave unchanged when nil" — the wire shape is
// JSON-omitempty + nullable so callers can patch any subset of fields.
type UpdateSurveyRequest struct {
	Name        *string `json:"name,omitempty" binding:"omitempty,min=1,max=200"`
	Description *string `json:"description,omitempty" binding:"omitempty,max=4096"`
	PrimaryMode *string `json:"primary_mode,omitempty" binding:"omitempty,oneof=form flow"`
}

// SaveVersionRequest is the body of POST /api/surveys/:id/versions.
//
// Schema is the raw graph JSON; the validator + DSL evaluator run on
// the bytes verbatim. Minor=true marks the new version as a backwards-
// compatible bump from the latest version of the same major.
type SaveVersionRequest struct {
	Schema json.RawMessage `json:"schema" binding:"required"`
	Minor  bool            `json:"minor"`
}

// PreviewRunRequest is the body of POST /api/surveys/:id/preview/run.
//
// The endpoint is stateless — Schema is parsed and evaluated in-place
// and no row is written. Answers is the per-call answer map keyed by
// node id; the handler converts each value into api.Answer via a small
// type-switch (see answersFromMap in handlers.go).
type PreviewRunRequest struct {
	Schema        json.RawMessage          `json:"schema" binding:"required"`
	CurrentNodeID string                   `json:"current_node_id" binding:"required"`
	Answers       map[string]AnswerPayload `json:"answers"`
}

// AnswerPayload is the wire shape of one answer the preview endpoint
// receives. Exactly one of SingleChoice / MultiChoice / Number / Text
// is populated, depending on the node's QuestionType. Mirrors
// api.Answer one-for-one so the conversion is mechanical.
type AnswerPayload struct {
	SingleChoice string   `json:"single_choice,omitempty"`
	MultiChoice  []string `json:"multi_choice,omitempty"`
	Number       *float64 `json:"number,omitempty"`
	Text         string   `json:"text,omitempty"`
	AnsweredAt   int64    `json:"answered_at,omitempty"`
}

// PreviewRunResponse is the body of a successful preview/run.
type PreviewRunResponse struct {
	NextNodeID string  `json:"next_node_id,omitempty"`
	Terminated bool    `json:"terminated"`
	EndKind    string  `json:"end_kind,omitempty"`
	Progress   float64 `json:"progress"`
}

// SurveyDTO is the wire shape for a survey. Stable transport-level
// type so future additions to api.Survey don't silently change the
// public response shape.
type SurveyDTO struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	PrimaryMode string    `json:"primary_mode"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	CreatedBy   string    `json:"created_by,omitempty"`
}

// VersionDTO is the wire shape for a survey version.
//
// Schema carries the canonical graph JSON; transport encodes it as
// json.RawMessage so the bytes round-trip without a re-marshal.
type VersionDTO struct {
	ID          string          `json:"id"`
	SurveyID    string          `json:"survey_id"`
	Major       int             `json:"major"`
	Minor       int             `json:"minor"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	IsActive    bool            `json:"is_active"`
	CreatedAt   time.Time       `json:"created_at"`
	CreatedBy   string          `json:"created_by,omitempty"`
	ActivatedAt *time.Time      `json:"activated_at,omitempty"`
}

// CreateSurveyResponse is the body of POST /api/surveys. The bare DTO
// would be ambiguous — wrapping it in {survey: ...} keeps room for
// adding meta-fields (warnings, audit-row id) without breaking the
// wire shape.
type CreateSurveyResponse struct {
	ID string `json:"id"`
}

// ListSurveysResponse is the body of GET /api/surveys.
type ListSurveysResponse struct {
	Surveys []SurveyDTO `json:"surveys"`
	Total   int         `json:"total"`
}

// ListVersionsResponse is the body of GET /api/surveys/:id/versions.
type ListVersionsResponse struct {
	Versions []VersionDTO `json:"versions"`
}

// ValidationIssueDTO is one line of a ValidationReportDTO.
type ValidationIssueDTO struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// ValidationReportDTO is the body returned for any validate /
// save-version failure that surfaces a structured report (422).
//
// Valid is always false on the failure path; the field is kept so the
// schema is uniform with future "preflight is OK" responses on the
// validate endpoint that may carry warnings.
type ValidationReportDTO struct {
	Valid  bool                 `json:"valid"`
	Issues []ValidationIssueDTO `json:"issues"`
}

// ErrorEnvelope is the JSON shape every 4xx/5xx response uses without
// a structured payload (validation reports use ValidationReportDTO
// instead). Mirrors auth/crm so the wire format stays uniform across
// modules.
type ErrorEnvelope struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// surveyToDTO converts an api.Survey into the wire-format SurveyDTO.
func surveyToDTO(s surveysapi.Survey) SurveyDTO {
	return SurveyDTO{
		ID:          s.ID.String(),
		TenantID:    s.TenantID.String(),
		Name:        s.Name,
		Description: s.Description,
		PrimaryMode: string(s.PrimaryMode),
		Status:      string(s.Status),
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
		CreatedBy:   uuidString(s.CreatedBy),
	}
}

// surveysToDTO is the slice form of surveyToDTO.
func surveysToDTO(in []surveysapi.Survey) []SurveyDTO {
	out := make([]SurveyDTO, len(in))
	for i, s := range in {
		out[i] = surveyToDTO(s)
	}
	return out
}

// versionToDTO converts an api.Version into the wire-format VersionDTO.
// The Schema field carries the raw JSON bytes verbatim — copying the
// slice protects callers against in-place mutation by gin's pool, but
// json.RawMessage already aliases the byte slice so the cost is one
// allocation per version.
func versionToDTO(v surveysapi.Version) VersionDTO {
	var raw json.RawMessage
	if len(v.Schema) > 0 {
		raw = make(json.RawMessage, len(v.Schema))
		copy(raw, v.Schema)
	}
	return VersionDTO{
		ID:          v.ID.String(),
		SurveyID:    v.SurveyID.String(),
		Major:       v.Major,
		Minor:       v.Minor,
		Schema:      raw,
		IsActive:    v.IsActive,
		CreatedAt:   v.CreatedAt,
		CreatedBy:   uuidString(v.CreatedBy),
		ActivatedAt: v.ActivatedAt,
	}
}

// versionsToDTO is the slice form of versionToDTO.
func versionsToDTO(in []surveysapi.Version) []VersionDTO {
	out := make([]VersionDTO, len(in))
	for i, v := range in {
		out[i] = versionToDTO(v)
	}
	return out
}

// reportToDTO converts a schemavalidator.ValidationReport (the source
// of truth for the validate endpoint) into the wire-format DTO.
func reportToDTO(r schemavalidator.ValidationReport) ValidationReportDTO {
	out := ValidationReportDTO{Valid: r.Valid}
	if len(r.Issues) > 0 {
		out.Issues = make([]ValidationIssueDTO, len(r.Issues))
		for i, iss := range r.Issues {
			out.Issues[i] = ValidationIssueDTO{
				Code:    iss.Code,
				Path:    iss.Path,
				Message: iss.Message,
			}
		}
	}
	return out
}

// apiReportToDTO converts the api.Report shape (carried by
// api.ValidationError on the SaveVersion path) into a
// ValidationReportDTO. The two shapes differ slightly — api.Issue uses
// NodeID where the validator uses Path — but the JSON encoding is
// uniform.
func apiReportToDTO(r surveysapi.Report) ValidationReportDTO {
	out := ValidationReportDTO{Valid: false}
	if len(r.Issues) > 0 {
		out.Issues = make([]ValidationIssueDTO, len(r.Issues))
		for i, iss := range r.Issues {
			out.Issues[i] = ValidationIssueDTO{
				Code:    iss.Code,
				Path:    iss.NodeID,
				Message: iss.Message,
			}
		}
	}
	return out
}

// uuidString renders a uuid.UUID as a canonical RFC 4122 string,
// returning the empty string for uuid.Nil so optional fields can be
// omitted via json:"...,omitempty".
func uuidString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

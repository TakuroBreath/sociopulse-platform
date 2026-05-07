// Package api defines public contracts for the surveys module.
// Other modules import only from this package — never from surveys/service or surveys/store.
//
// surveys owns survey definitions and immutable versions, JSON schema
// validation, graph validation (unreachability, cycle-without-exit, dangling
// edges, forward refs in DSL), the DSL evaluator (expr-lang/expr subset),
// the runtime (next-node + answer validation + progress estimate), and
// version-activation atomicity.
package api

import (
	"time"

	"github.com/google/uuid"
)

// PrimaryMode enumerates the two delivery modes for a survey.
type PrimaryMode string

const (
	// ModeForm is a single-page form with all questions visible at once.
	ModeForm PrimaryMode = "form"
	// ModeFlow is a guided flow with one question per screen, branching by DSL.
	ModeFlow PrimaryMode = "flow"
)

// SurveyStatus enumerates the lifecycle states of a survey.
type SurveyStatus string

const (
	StatusActive   SurveyStatus = "active"
	StatusArchived SurveyStatus = "archived"
)

// EndKind labels the terminal node a survey concluded on.
type EndKind string

const (
	EndKindSuccess EndKind = "success"
	EndKindRefusal EndKind = "refusal"
	EndKindNone    EndKind = ""
)

// QuestionType enumerates the answer kinds the runtime knows how to validate.
type QuestionType string

const (
	TypeSingle QuestionType = "single"
	TypeMulti  QuestionType = "multi"
	TypeNumber QuestionType = "number"
	TypeText   QuestionType = "text"
	TypeSelect QuestionType = "select"
)

// NodeKind enumerates the node kinds in a survey graph.
type NodeKind string

const (
	NodeStart      NodeKind = "start"
	NodeIntro      NodeKind = "intro"
	NodeQuestion   NodeKind = "question"
	NodeTextBlock  NodeKind = "text-block"
	NodeSuccessEnd NodeKind = "success-end"
	NodeRefusalEnd NodeKind = "refusal-end"
	NodeCondition  NodeKind = "condition"
	NodeJump       NodeKind = "jump"
)

// Survey is the public projection of a surveys row (the "definition" — a
// container for versions). PrimaryMode is fixed for the lifetime of the survey.
type Survey struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Name        string
	Description string
	PrimaryMode PrimaryMode
	Status      SurveyStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CreatedBy   uuid.UUID
}

// Version is one immutable schema bound to a Survey. Major bumps replace the
// schema completely; minor bumps are backwards-compatible. Activation flips
// IsActive=true on the chosen version atomically (Activate transaction).
type Version struct {
	ID          uuid.UUID
	SurveyID    uuid.UUID
	Major       int
	Minor       int
	Schema      []byte // canonical JSON of the survey graph
	IsActive    bool
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
	ActivatedAt *time.Time
}

// CreateSurveyInput is the payload for SurveyService.Create.
type CreateSurveyInput struct {
	Name        string
	Description string
	PrimaryMode PrimaryMode
}

// UpdateSurveyInput patches survey metadata (not the schema).
type UpdateSurveyInput struct {
	Name        *string
	Description *string
	PrimaryMode *PrimaryMode
}

// ListFilter narrows SurveyService.List.
type ListFilter struct {
	Status SurveyStatus
	Search string
	Limit  int
	Offset int
}

// Answer is one answer the runtime receives from the operator UI. Exactly
// one of SingleChoice / MultiChoice / Number / Text is populated, depending
// on the node's QuestionType.
type Answer struct {
	NodeID       string
	SingleChoice string
	MultiChoice  []string
	Number       *float64
	Text         string
	AnsweredAt   int64 // unix millis
}

// NodeResult is the return of Runtime.NextNode. NextNodeID is empty when
// Terminated=true.
type NodeResult struct {
	NextNodeID string
	Terminated bool
	EndKind    EndKind // success | refusal | "" if not terminated
	Progress   float64 // [0,1]
}

// AnswerKey identifies one answer cell in the per-call answer store.
type AnswerKey struct {
	CallID uuid.UUID
	NodeID string
}

// Report is the structured validation report attached to a SaveVersion failure.
// The HTTP layer uses errors.As(err, &ValidationError) to surface it.
type Report struct {
	Issues []Issue
}

// Issue is one validation finding.
type Issue struct {
	Code    string // "cycle", "unreachable", "dangling-edge", ...
	NodeID  string
	Message string
}

// ValidationError wraps the structured Report as an error so the HTTP layer
// can surface it with errors.As. Unwrap returns ErrValidation so
// errors.Is(err, ErrValidation) still matches.
type ValidationError struct {
	Report Report
}

// Error returns the sentinel message; structured details are exposed via Report.
func (v *ValidationError) Error() string { return ErrValidation.Error() }

// Unwrap chains to ErrValidation so errors.Is matches.
func (v *ValidationError) Unwrap() error { return ErrValidation }

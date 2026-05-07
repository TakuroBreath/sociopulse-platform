package api

import (
	"context"

	"github.com/google/uuid"
)

// SurveyService is the public CRUD surface for surveys + versions.
type SurveyService interface {
	// Create allocates a new survey container; it has no versions until SaveVersion.
	Create(ctx context.Context, in CreateSurveyInput) (uuid.UUID, error)
	// Get returns the survey with the given ID, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Survey, error)
	// List returns surveys matching filter.
	List(ctx context.Context, filter ListFilter) ([]Survey, error)
	// Update patches survey metadata.
	Update(ctx context.Context, id uuid.UUID, in UpdateSurveyInput) error
	// Archive transitions the survey to StatusArchived.
	Archive(ctx context.Context, id uuid.UUID) error
	// SaveVersion validates and stores a new schema. minor=true marks the
	// new version as a backwards-compatible bump from the latest version
	// of the same major. On validation failure, returns *ValidationError.
	SaveVersion(ctx context.Context, surveyID uuid.UUID, schemaJSON []byte, minor bool) (Version, error)
	// Activate atomically flips IsActive=true on versionID and false on every
	// other version of surveyID.
	Activate(ctx context.Context, surveyID, versionID uuid.UUID) error
	// GetActiveVersion returns the currently active version, or ErrNoActiveVersion.
	GetActiveVersion(ctx context.Context, surveyID uuid.UUID) (Version, error)
	// ListVersions returns every version of the survey, newest first.
	ListVersions(ctx context.Context, surveyID uuid.UUID) ([]Version, error)
}

// VersionStore is the persistence surface for versions, lifted out so the
// service can be unit-tested with an in-memory implementation.
type VersionStore interface {
	// SaveVersion inserts a row. Caller must have validated already.
	SaveVersion(ctx context.Context, v Version) error
	// GetVersion returns the version with the given ID.
	GetVersion(ctx context.Context, id uuid.UUID) (Version, error)
	// GetActive returns the currently active version, or ErrNoActiveVersion.
	GetActive(ctx context.Context, surveyID uuid.UUID) (Version, error)
	// ListVersions returns every version of surveyID, newest first.
	ListVersions(ctx context.Context, surveyID uuid.UUID) ([]Version, error)
	// Activate flips IsActive atomically (single transaction).
	Activate(ctx context.Context, surveyID, versionID uuid.UUID) error
}

// Runtime is the per-call survey evaluator. Stateless: every call passes
// the schema bytes and the answers map. Designed to be compiled to wasm
// for browser preview (ADR-0008).
type Runtime interface {
	// NextNode evaluates the DSL on the current node and returns the next
	// node + termination state.
	NextNode(schema []byte, currentNodeID string, answers map[string]Answer) (NodeResult, error)
	// ValidateAnswer checks that ans is well-formed for the given node's QuestionType.
	ValidateAnswer(schema []byte, nodeID string, ans Answer) error
	// CalculateProgress returns a [0,1] progress estimate for the current node.
	CalculateProgress(schema []byte, currentNodeID string) (float64, error)
}

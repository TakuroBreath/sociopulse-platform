package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// SurveyStorePort is the persistence contract surveys/service.SurveyService
// consumes for surveys rows. It lives in api/ (not in store/) so the
// service package depends only on abstractions and tests can supply a
// hand-rolled fake without crossing the depguard module boundary
// (`internal/surveys/service` cannot import `internal/surveys/store`).
//
// All methods accept a postgres.Tx so the service layer co-locates the
// row write with the audit row and any future outbox row in the same
// transaction. Read methods take the same Tx — the service typically
// opens a per-tenant transaction (pool.WithTenant) and chains the read
// through it so the same RLS policy applies.
type SurveyStorePort interface {
	// Insert writes a new surveys row. The supplied Survey.ID may be
	// uuid.Nil — the store relies on the column DEFAULT to mint a fresh
	// id and returns the row populated with id+timestamps. Returns
	// ErrNameTaken on (tenant_id, lower(name)) unique violation when a
	// future migration adds that constraint; today the service-layer
	// pre-check enforces uniqueness and the store surfaces other unique
	// violations as raw errors.
	Insert(ctx context.Context, tx postgres.Tx, s Survey) (Survey, error)

	// GetByID returns the survey with the supplied id (regardless of
	// archive state). Returns ErrNotFound when the row is absent or RLS
	// hides it.
	GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (Survey, error)

	// List returns one page of surveys plus the unfiltered total count
	// for the tenant filter. Pagination clamping is the service layer's
	// responsibility; the store treats Limit/Offset as authoritative.
	List(ctx context.Context, tx postgres.Tx, f ListFilter) (rows []Survey, total int64, err error)

	// Update applies a partial-update patch via COALESCE so only non-nil
	// fields in patch are written. Returns ErrNotFound when the row is
	// missing or already archived. The returned Survey carries the
	// post-update column values (RETURNING *).
	Update(ctx context.Context, tx postgres.Tx, id uuid.UUID, patch SurveyPatch) (Survey, error)

	// Archive flips the row to status=archived and stamps archived_at.
	// Returns ErrNotFound when the row is missing.
	Archive(ctx context.Context, tx postgres.Tx, id uuid.UUID, at time.Time) error

	// SetCurrentVersion updates surveys.current_version_id on activation.
	// Both ids must reference rows owned by the same tenant; the FK
	// constraint and RLS together enforce that.
	SetCurrentVersion(ctx context.Context, tx postgres.Tx, surveyID, versionID uuid.UUID) error
}

// SurveyPatch carries the partial-update fields for SurveyStorePort.Update.
// nil means "leave the column untouched" so a single SQL statement covers
// every subset of editable fields.
type SurveyPatch struct {
	Name        *string
	Description *string
	PrimaryMode *PrimaryMode
}

// IsEmpty reports whether the patch carries any change. Used by the
// service layer to short-circuit no-op updates before opening a tx.
func (p SurveyPatch) IsEmpty() bool {
	return p.Name == nil && p.Description == nil && p.PrimaryMode == nil
}

// VersionStorePort is the persistence contract surveys/service consumes
// for survey_versions rows. The narrow interface mirrors SurveyStorePort:
// every method accepts a postgres.Tx so the service layer can co-locate
// the row write with audit + outbox writes in the same transaction.
//
// Cross-module callers MUST import from internal/surveys/api only —
// depguard's module-boundaries rule rejects any direct import of
// internal/surveys/store from other modules.
type VersionStorePort interface {
	// Insert writes a new survey_versions row. The supplied Version.ID
	// may be uuid.Nil — the store relies on the column DEFAULT to mint a
	// fresh id and returns the row populated with id+timestamps. The
	// caller is responsible for computing major/minor; the store surfaces
	// (survey_id, major, minor) unique-violations as raw errors so the
	// caller can decide how to retry.
	Insert(ctx context.Context, tx postgres.Tx, v Version) (Version, error)

	// GetByID returns the version with the supplied id. Returns
	// ErrVersionNotFound when the row is absent or RLS hides it.
	GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (Version, error)

	// GetActive returns the active version of surveyID. Returns
	// ErrNoActiveVersion when no version is currently flagged is_active.
	GetActive(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) (Version, error)

	// List returns every version of surveyID, newest first by (major,
	// minor) then created_at.
	List(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) ([]Version, error)

	// LatestMajor returns the maximum `major` across all versions of
	// surveyID, or 0 when the survey has no versions yet (the next major
	// bump is 1 in that case).
	LatestMajor(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) (int, error)

	// LatestMinor returns the maximum `minor` for (surveyID, major), or
	// 0 when no version exists for that major.
	LatestMinor(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID, major int) (int, error)

	// DeactivateAll flips is_active=false on every version of surveyID.
	// Used by the Activate flow as the first step (the partial unique
	// index `survey_versions_active_one` makes a "set both" UPDATE
	// impossible — we deactivate first, then activate the target).
	DeactivateAll(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) error

	// Activate flips is_active=true on versionID and stamps
	// activated_at=at. The caller MUST have already deactivated every
	// other version of the survey (via DeactivateAll) inside the same
	// transaction; otherwise the partial unique index raises 23505.
	Activate(ctx context.Context, tx postgres.Tx, versionID uuid.UUID, at time.Time) error
}

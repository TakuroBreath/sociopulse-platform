package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// ProjectStorePort is the persistence contract crm/service.ProjectService
// consumes for project rows. It lives in api/ (not in store/) so the
// service package depends only on abstractions and tests can supply a
// hand-rolled fake without crossing the depguard module boundary
// (`internal/crm/service` cannot import `internal/crm/store`).
//
// All methods accept a postgres.Tx so the service layer co-locates the
// row write with the audit row and any future outbox row in the same
// transaction. Read methods take the same Tx — the service typically
// opens a per-tenant transaction (pool.WithTenant) and chains the read
// through it so the same RLS policy applies.
type ProjectStorePort interface {
	// Insert writes a new projects row. The supplied Project.ID may be
	// uuid.Nil — the store relies on the column DEFAULT to mint a fresh
	// id and returns the row populated with id+timestamps. Returns
	// ErrProjectCodeTaken on (tenant_id, code) unique violation.
	Insert(ctx context.Context, tx postgres.Tx, p Project) (Project, error)

	// GetByID returns the project with the supplied id (regardless of
	// archive state). Returns ErrProjectNotFound when the row is absent
	// or RLS hides it.
	GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (Project, error)

	// GetByCode returns the project matching (tenantID, code). Returns
	// ErrProjectNotFound when missing.
	GetByCode(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, code string) (Project, error)

	// List returns one page of projects plus the unfiltered total count
	// for the tenant filter. Pagination clamping is the service layer's
	// responsibility; the store treats Limit/Offset as authoritative.
	List(ctx context.Context, tx postgres.Tx, f ListProjectsFilter) (rows []Project, total int64, err error)

	// Update applies a partial-update patch via COALESCE so only non-nil
	// fields in patch are written. Returns ErrProjectNotFound when the row
	// is missing or already archived. The returned Project carries the
	// post-update column values (RETURNING *).
	Update(ctx context.Context, tx postgres.Tx, id uuid.UUID, patch UpdatePatch) (Project, error)

	// UpdateStatus rewrites projects.status (and archived_at when the
	// caller passes a non-nil archivedAt) for the supplied id. Returns
	// the updated row. Returns ErrProjectNotFound on a missing row.
	// Service-layer state-machine guards (Active|Paused only) live in
	// the service, not here.
	UpdateStatus(ctx context.Context, tx postgres.Tx, id uuid.UUID, newStatus ProjectStatus, archivedAt *time.Time) (Project, error)

	// AggregateProgress returns the live counter snapshot used by the
	// dashboard widget — one row per (status, project_id) join over
	// respondents, plus the per-quota-cell breakdown when the project
	// has any project_quotas rows. Reads only; never writes.
	AggregateProgress(ctx context.Context, tx postgres.Tx, projectID uuid.UUID) (ProjectProgress, error)

	// AssignOperators MERGE-inserts assignments for the supplied
	// project. Existing rows are kept (ON CONFLICT DO NOTHING), so the
	// returned `added` is the number of operators that became new
	// members. Service layer pre-deduplicates the input slice; the
	// store takes the slice authoritatively.
	AssignOperators(ctx context.Context, tx postgres.Tx, projectID uuid.UUID, operatorIDs []uuid.UUID) (added int, err error)

	// UnassignOperator deletes one assignment row. Returns deleted=true
	// when the row was present (and removed); deleted=false when the
	// operator was never assigned (no-op).
	UnassignOperator(ctx context.Context, tx postgres.Tx, projectID uuid.UUID, operatorID uuid.UUID) (deleted bool, err error)

	// ListMembers returns the operators currently assigned to projectID
	// joined with users for display fields. Sorted by assigned_at ASC so
	// the UI renders members in the order they joined the project.
	ListMembers(ctx context.Context, tx postgres.Tx, projectID uuid.UUID) ([]ProjectMember, error)
}

// RespondentStorePort is the persistence contract crm/service consumes
// for respondents rows. Mirrors ProjectStorePort: every method accepts
// a postgres.Tx so the service layer co-locates the row write with
// audit, DNC checks, and any future outbox row in the same transaction.
//
// Cross-module callers MUST import from internal/crm/api only —
// depguard's module-boundaries rule rejects any direct import of
// internal/crm/store from other modules.
type RespondentStorePort interface {
	// Insert writes a fresh respondents row. The supplied
	// Respondent.ID may be uuid.Nil; the store relies on the column
	// DEFAULT to mint a fresh id and returns the row populated with
	// id+timestamp. Returns ErrDuplicateRespondent on the
	// (tenant_id, project_id, phone_hash) unique-constraint violation
	// (000006_respondents_uniq.up.sql).
	Insert(ctx context.Context, tx postgres.Tx, r Respondent) (Respondent, error)

	// GetByID returns the respondent with the supplied id. Returns
	// ErrRespondentNotFound when the row is absent or RLS hides it.
	GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (Respondent, error)

	// GetByHash returns the respondent matching (tenantID, projectID,
	// phoneHash). Returns ErrRespondentNotFound when no row matches.
	// Used by Create to short-circuit the unique-constraint round-trip
	// with a friendlier error path; the unique constraint remains the
	// authoritative dup detector.
	GetByHash(ctx context.Context, tx postgres.Tx, tenantID, projectID uuid.UUID, phoneHash []byte) (Respondent, error)

	// IsBlockedDNC reports whether phoneHash appears in project_dnc for
	// the supplied (tenantID, projectID) — counting both project-scoped
	// entries and tenant-wide entries (project_id IS NULL). Pure read;
	// no audit row, no event publish.
	IsBlockedDNC(ctx context.Context, tx postgres.Tx, tenantID, projectID uuid.UUID, phoneHash []byte) (bool, error)

	// InsertBatch bulk-inserts respondents using the PostgreSQL COPY
	// protocol (10x-100x faster than per-row INSERT). The supplied rows
	// must already be deduplicated against the existing project — the
	// caller is responsible for filtering on (tenant_id, project_id,
	// phone_hash) collisions BEFORE this call (see ExistingHashes).
	// Returns the number of rows actually written; zero rows is a no-op
	// that returns (0, nil) without opening a copy stream.
	//
	// On a UNIQUE constraint violation the entire COPY operation rolls
	// back — CopyFrom does NOT support ON CONFLICT. The error is wrapped
	// so callers can errors.Is(err, ErrDuplicateRespondent) and retry
	// after re-running ExistingHashes.
	InsertBatch(ctx context.Context, tx postgres.Tx, rows []Respondent) (inserted int, err error)

	// ExistingHashes returns the subset of `hashes` that already exist
	// for (tenantID, projectID). Callers use this to dedupe their batch
	// against rows already in Postgres before InsertBatch runs.
	//
	// The query is "SELECT phone_hash FROM respondents WHERE
	// tenant_id = $1 AND project_id = $2 AND phone_hash = ANY($3)", so
	// the cost is one round-trip plus a partial index scan on the
	// (tenant_id, project_id, phone_hash) UNIQUE index. Empty input
	// returns (nil, nil) without a query.
	ExistingHashes(ctx context.Context, tx postgres.Tx, tenantID, projectID uuid.UUID, hashes [][]byte) ([][]byte, error)

	// SoftDelete stamps deleted_at + deletion_reason on the row at the
	// supplied id. The row stays visible to admin tooling but is hidden
	// from operator surfaces (Get/Search filter on deleted_at IS NULL).
	// Returns ErrRespondentNotFound when no row matched (id missing or
	// already soft-deleted by a concurrent caller). Idempotent against
	// the same row only when the caller supplies the same reason — a
	// second call with a different reason will overwrite the column,
	// which the service layer prevents by short-circuiting on the
	// pre-existing deleted_at via GetByID.
	SoftDelete(ctx context.Context, tx postgres.Tx, id uuid.UUID, reason string, at time.Time) error

	// PurgeOlderThan hard-deletes up to `limit` respondents whose
	// deleted_at < cutoff and returns the ids that were removed. The
	// daily purge worker calls this once per tick; the per-row id list
	// lets the caller emit one audit row per purged id without a
	// follow-up SELECT.
	//
	// Implementation MUST use a single DELETE ... LIMIT $2 RETURNING id
	// so concurrent purgers can't double-delete the same row. Empty
	// result returns (nil, nil); a zero limit returns (nil, nil) without
	// running a query.
	PurgeOlderThan(ctx context.Context, tx postgres.Tx, cutoff time.Time, limit int) (purgedIDs []uuid.UUID, err error)

	// Search returns one page of respondents matching f, plus the total
	// count of matching rows. The query filters out soft-deleted rows
	// (deleted_at IS NULL) by default; admin tooling that needs to see
	// pending-purge rows uses the dedicated admin path (not Search).
	//
	// Pagination is via Page (1-based) + PageSize; the service layer
	// clamps both to the documented bounds before calling the store.
	// Sort order is created_at DESC so newest rows surface first.
	Search(ctx context.Context, tx postgres.Tx, f SearchRespondentsFilter) (rows []Respondent, total int64, err error)
}

// UpdatePatch carries the partial-update fields for ProjectStorePort.Update.
// Pointer-typed fields denote "leave unchanged when nil"; the store renders
// each field via COALESCE($n, col) so the SQL stays one round-trip.
type UpdatePatch struct {
	Name        *string
	Customer    *string
	TargetCount *int
	PeriodFrom  *time.Time
	PeriodTo    *time.Time
	SurveyID    *uuid.UUID
}

// IsEmpty reports whether patch carries no field overrides. The service
// layer uses this to short-circuit a no-op Update call (no SQL, no audit
// row).
func (p UpdatePatch) IsEmpty() bool {
	return p.Name == nil &&
		p.Customer == nil &&
		p.TargetCount == nil &&
		p.PeriodFrom == nil &&
		p.PeriodTo == nil &&
		p.SurveyID == nil
}

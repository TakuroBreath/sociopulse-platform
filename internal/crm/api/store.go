package api

import (
	"context"

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
}

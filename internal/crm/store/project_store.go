package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// pgUniqueViolation is the SQLSTATE code Postgres returns for a unique-
// constraint violation. Translated to api.ErrProjectCodeTaken for the
// (tenant_id, code) uniqueness invariant on the projects table.
const pgUniqueViolation = "23505"

// ProjectStore is the Postgres-backed implementation of api.ProjectStorePort.
//
// Mutating methods accept a postgres.Tx so the crm service layer can co-
// locate the row write with audit and outbox writes in the same
// transaction. Read methods take the same Tx — the service is expected
// to open a per-tenant transaction (Pool.WithTenant) and chain every
// store call through it so the RLS policy applies uniformly.
//
// Cross-module callers MUST import from internal/crm/api only;
// depguard's module-boundaries rule rejects direct imports of this
// package from outside the crm module.
type ProjectStore struct {
	pool *postgres.Pool
}

// Compile-time assertion that *ProjectStore satisfies api.ProjectStorePort.
var _ api.ProjectStorePort = (*ProjectStore)(nil)

// NewProjectStore constructs a ProjectStore. The pool reference is held
// for symmetry with the auth/store and tenancy/store packages — the
// current methods all operate on the supplied Tx, so the pool is unused
// at every call site. Future read paths that need an internal BypassRLS
// tx will use it.
func NewProjectStore(pool *postgres.Pool) *ProjectStore {
	return &ProjectStore{pool: pool}
}

// projectColumns is the canonical projection used by every read query so
// the field order matches scanRow without drift across call sites.
const projectColumns = `id, tenant_id, code, name, customer, status,
	target_count, period_from, period_to, survey_id,
	default_survey_version_id, is_advertising, created_by,
	created_at, updated_at, archived_at`

// rowScanner abstracts pgx.Row and a single pgx.Rows step so scanRow can
// be reused across QueryRow and rows.Next loops.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRow fills an api.Project from a single row, normalising the
// nullable timestamps and the optional FK columns.
func scanRow(r rowScanner) (api.Project, error) {
	var (
		p      api.Project
		status string
	)
	err := r.Scan(
		&p.ID, &p.TenantID, &p.Code, &p.Name, &p.Customer, &status,
		&p.TargetCount, &p.PeriodFrom, &p.PeriodTo, &p.SurveyID,
		&p.DefaultSurveyVersionID, &p.IsAdvertising, &p.CreatedBy,
		&p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt,
	)
	if err != nil {
		return api.Project{}, err
	}
	p.Status = api.ProjectStatus(status)
	return p, nil
}

// translateErr maps pgx / pgconn errors into the crm api sentinels.
// pgx.ErrNoRows -> ErrProjectNotFound; SQLSTATE 23505 (unique violation)
// on the (tenant_id, code) constraint -> ErrProjectCodeTaken.
func translateErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ErrProjectNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		// Wrap the constraint name into the sentinel via errors.Join
		// so callers can errors.Is(err, api.ErrProjectCodeTaken)
		// without losing the diagnostic constraint detail.
		return errors.Join(api.ErrProjectCodeTaken, fmt.Errorf("constraint=%s", pgErr.ConstraintName))
	}
	return err
}

// Insert implements api.ProjectStorePort.Insert. The supplied
// Project.ID is ignored — Postgres mints a fresh id via gen_random_uuid()
// and the returned row carries the canonical id+timestamps. Status
// defaults to api.StatusActive when zero so the service layer doesn't
// have to repeat that boilerplate; the DB-level CHECK constraint also
// enforces it.
func (s *ProjectStore) Insert(ctx context.Context, tx postgres.Tx, p api.Project) (api.Project, error) {
	const q = `
		INSERT INTO projects (
			tenant_id, code, name, customer, status, target_count,
			period_from, period_to, survey_id, is_advertising, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING ` + projectColumns

	status := p.Status
	if status == "" {
		status = api.StatusActive
	}

	saved, err := scanRow(tx.QueryRow(ctx, q,
		p.TenantID,
		p.Code,
		p.Name,
		p.Customer,
		string(status),
		p.TargetCount,
		p.PeriodFrom,
		p.PeriodTo,
		p.SurveyID,
		p.IsAdvertising,
		p.CreatedBy,
	))
	if err != nil {
		if terr := translateErr(err); errors.Is(terr, api.ErrProjectCodeTaken) {
			return api.Project{}, terr
		}
		return api.Project{}, fmt.Errorf("crm/store: insert project: %w", err)
	}
	return saved, nil
}

// GetByID implements api.ProjectStorePort.GetByID inside the caller's tx.
func (s *ProjectStore) GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (api.Project, error) {
	const q = `SELECT ` + projectColumns + ` FROM projects WHERE id = $1`

	p, err := scanRow(tx.QueryRow(ctx, q, id))
	if err != nil {
		if terr := translateErr(err); errors.Is(terr, api.ErrProjectNotFound) {
			return api.Project{}, terr
		}
		return api.Project{}, fmt.Errorf("crm/store: get project by id: %w", err)
	}
	return p, nil
}

// GetByCode implements api.ProjectStorePort.GetByCode inside the caller's
// tx. Lookup is case-insensitive: the partial index
// idx_projects_tenant_code_lower covers the (tenant_id, lower(code))
// predicate. The unique constraint is case-sensitive at the DB level, so
// this method is informational rather than authoritative for the
// duplicate-code check (Insert raises the canonical sentinel via
// constraint violation).
func (s *ProjectStore) GetByCode(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, code string) (api.Project, error) {
	const q = `
		SELECT ` + projectColumns + `
		FROM projects
		WHERE tenant_id = $1 AND lower(code) = lower($2)`

	p, err := scanRow(tx.QueryRow(ctx, q, tenantID, code))
	if err != nil {
		if terr := translateErr(err); errors.Is(terr, api.ErrProjectNotFound) {
			return api.Project{}, terr
		}
		return api.Project{}, fmt.Errorf("crm/store: get project by code: %w", err)
	}
	return p, nil
}

// List implements api.ProjectStorePort.List inside the caller's tx. The
// total count comes from a second query so the result is the unfiltered
// row count for the (tenant_id, archived_at, status, search) predicate
// (used to drive admin pagination counters). Both queries respect the
// same archived-or-not predicate so total stays consistent with rows.
//
// The dynamic where clause is built from explicit positional placeholders
// — never via string interpolation of user-supplied Search/Status — so
// SQL injection is not reachable through the filter surface.
func (s *ProjectStore) List(ctx context.Context, tx postgres.Tx, f api.ListProjectsFilter) ([]api.Project, int64, error) {
	clause, args := buildListPredicate(f)

	listQ := `
		SELECT ` + projectColumns + `
		FROM projects
		WHERE ` + clause + `
		ORDER BY created_at DESC
		LIMIT $` + intArg(len(args)+1) + ` OFFSET $` + intArg(len(args)+2)

	countQ := `
		SELECT count(*)
		FROM projects
		WHERE ` + clause

	listArgs := append(append([]any{}, args...), f.Limit, f.Offset)

	rows, err := tx.Query(ctx, listQ, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("crm/store: list projects query: %w", err)
	}
	defer rows.Close()

	out := make([]api.Project, 0)
	for rows.Next() {
		p, err := scanRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("crm/store: list projects scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("crm/store: list projects iterate: %w", err)
	}

	var total int64
	if err := tx.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("crm/store: list projects count: %w", err)
	}
	return out, total, nil
}

// buildListPredicate constructs the parameterised WHERE clause for List
// from the filter. The returned args slice is positional; placeholders
// $1..$N reference args[0..N-1] in order. Limit/Offset are NOT included
// — callers append those at the end of the args slice for the SELECT
// query (the count query uses args as-is).
func buildListPredicate(f api.ListProjectsFilter) (string, []any) {
	args := []any{f.TenantID}
	predicates := []string{"tenant_id = $1"}

	if !f.IncludeArchived {
		predicates = append(predicates, "archived_at IS NULL")
	}
	if f.Status != nil {
		args = append(args, string(*f.Status))
		predicates = append(predicates, "status = $"+intArg(len(args)))
	}
	if trimmed := strings.TrimSpace(f.Search); trimmed != "" {
		args = append(args, "%"+strings.ToLower(trimmed)+"%")
		idx := intArg(len(args))
		predicates = append(predicates,
			"(lower(code) LIKE $"+idx+
				" OR lower(name) LIKE $"+idx+
				" OR lower(customer) LIKE $"+idx+")")
	}

	return strings.Join(predicates, " AND "), args
}

// intArg formats an int as a positional placeholder index ($1, $2, ...).
// Inlined here rather than reaching for strconv at every call site —
// keeps the SQL-building helper readable.
func intArg(i int) string {
	// Hand-rolled to avoid pulling strconv just for placeholder math.
	if i < 10 {
		return string(rune('0' + i))
	}
	// Fallback: positional indexes >9 are exceedingly rare in our queries
	// (we never have more than a handful of filter args), but stay
	// correct anyway.
	return fmt.Sprintf("%d", i)
}

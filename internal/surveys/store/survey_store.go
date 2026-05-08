package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// pgUniqueViolation is the SQLSTATE code Postgres returns for a unique-
// constraint violation. The surveys table has no unique constraint on
// name today; the constant is here for symmetry with crm/store and for
// the day a future migration introduces one.
const pgUniqueViolation = "23505"

// SurveyStore is the Postgres-backed implementation of api.SurveyStorePort.
//
// Mutating methods accept a postgres.Tx so the surveys/service layer can
// co-locate the row write with audit and outbox writes in the same
// transaction. Read methods take the same Tx — the service is expected
// to open a per-tenant transaction (Pool.WithTenant) and chain every
// store call through it so the RLS policy applies uniformly.
//
// Cross-module callers MUST import from internal/surveys/api only;
// depguard's module-boundaries rule rejects direct imports of this
// package from outside the surveys module.
type SurveyStore struct {
	pool *postgres.Pool
}

// Compile-time assertion that *SurveyStore satisfies api.SurveyStorePort.
var _ api.SurveyStorePort = (*SurveyStore)(nil)

// NewSurveyStore constructs a SurveyStore. The pool reference is held
// for symmetry with the auth/store and crm/store packages — the current
// methods all operate on the supplied Tx, so the pool is unused at every
// call site. Future read paths that need an internal BypassRLS tx will
// use it.
func NewSurveyStore(pool *postgres.Pool) *SurveyStore {
	return &SurveyStore{pool: pool}
}

// surveyColumns is the canonical projection used by every read query so
// the field order matches scanSurvey without drift across call sites.
const surveyColumns = `id, tenant_id, name, description, primary_mode,
	status, created_at, updated_at, created_by`

// surveyRowScanner abstracts pgx.Row and a single pgx.Rows step so
// scanSurvey can be reused across QueryRow and rows.Next loops.
type surveyRowScanner interface {
	Scan(dest ...any) error
}

// scanSurvey fills an api.Survey from a single row. The created_by
// column is nullable in the DB (FK to users(id) with no constraint that
// every survey carries a creator); we scan into a *uuid.UUID and
// project nil to uuid.Nil so the api.Survey DTO stays non-pointer.
func scanSurvey(r surveyRowScanner) (api.Survey, error) {
	var (
		s         api.Survey
		mode      string
		status    string
		createdBy *uuid.UUID
	)
	err := r.Scan(
		&s.ID, &s.TenantID, &s.Name, &s.Description, &mode,
		&status, &s.CreatedAt, &s.UpdatedAt, &createdBy,
	)
	if err != nil {
		return api.Survey{}, err
	}
	s.PrimaryMode = api.PrimaryMode(mode)
	s.Status = api.SurveyStatus(status)
	if createdBy != nil {
		s.CreatedBy = *createdBy
	}
	return s, nil
}

// translateSurveyErr maps pgx / pgconn errors into the surveys api
// sentinels. pgx.ErrNoRows -> ErrNotFound. SQLSTATE 23505 is currently
// surfaced as-is because the surveys table carries no unique
// constraint on (tenant_id, name); a future migration that adds one
// would extend this function with constraint-name discrimination
// matching the crm/store pattern.
func translateSurveyErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ErrNotFound
	}
	return err
}

// Insert implements api.SurveyStorePort.Insert. The supplied Survey.ID
// is ignored — Postgres mints a fresh id via gen_random_uuid() and the
// returned row carries the canonical id+timestamps. Status defaults to
// api.StatusActive when zero so the service layer doesn't have to
// repeat that boilerplate; the DB-level CHECK constraint also enforces
// the allowed values.
//
// The created_by column accepts NULL — when the supplied Survey.CreatedBy
// is uuid.Nil, we write NULL so the FK to users(id) is not violated by
// a synthetic zero UUID.
func (s *SurveyStore) Insert(ctx context.Context, tx postgres.Tx, in api.Survey) (api.Survey, error) {
	const q = `
		INSERT INTO surveys (
			tenant_id, name, description, primary_mode, status, created_by
		) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + surveyColumns

	mode := in.PrimaryMode
	if mode == "" {
		mode = api.ModeForm
	}
	status := in.Status
	if status == "" {
		status = api.StatusActive
	}

	var createdBy any
	if in.CreatedBy != uuid.Nil {
		createdBy = in.CreatedBy
	}

	saved, err := scanSurvey(tx.QueryRow(ctx, q,
		in.TenantID,
		in.Name,
		in.Description,
		string(mode),
		string(status),
		createdBy,
	))
	if err != nil {
		if terr := translateSurveyErr(err); errors.Is(terr, api.ErrNotFound) {
			return api.Survey{}, terr
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return api.Survey{}, fmt.Errorf("surveys/store: insert survey unique violation: %w (constraint=%s)", err, pgErr.ConstraintName)
		}
		return api.Survey{}, fmt.Errorf("surveys/store: insert survey: %w", err)
	}
	return saved, nil
}

// GetByID implements api.SurveyStorePort.GetByID inside the caller's tx.
func (s *SurveyStore) GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (api.Survey, error) {
	const q = `SELECT ` + surveyColumns + ` FROM surveys WHERE id = $1`

	survey, err := scanSurvey(tx.QueryRow(ctx, q, id))
	if err != nil {
		if terr := translateSurveyErr(err); errors.Is(terr, api.ErrNotFound) {
			return api.Survey{}, terr
		}
		return api.Survey{}, fmt.Errorf("surveys/store: get survey by id: %w", err)
	}
	return survey, nil
}

// List implements api.SurveyStorePort.List. The total count comes from
// a second query so the result is the unfiltered row count for the
// (tenant_id, status, search) predicate (used to drive admin
// pagination counters). Both queries respect the same archived-or-not
// predicate so total stays consistent with rows.
//
// The dynamic where clause is built from explicit positional placeholders
// — never via string interpolation of user-supplied Search/Status — so
// SQL injection is not reachable through the filter surface.
func (s *SurveyStore) List(ctx context.Context, tx postgres.Tx, f api.ListFilter) ([]api.Survey, int64, error) {
	clause, args := buildSurveyListPredicate(f)

	listQ := `
		SELECT ` + surveyColumns + `
		FROM surveys
		WHERE ` + clause + `
		ORDER BY created_at DESC
		LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)

	countQ := `
		SELECT count(*)
		FROM surveys
		WHERE ` + clause

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	listArgs := append(append([]any{}, args...), limit, offset)

	rows, err := tx.Query(ctx, listQ, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("surveys/store: list surveys query: %w", err)
	}
	defer rows.Close()

	out := make([]api.Survey, 0)
	for rows.Next() {
		row, err := scanSurvey(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("surveys/store: list surveys scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("surveys/store: list surveys iterate: %w", err)
	}

	var total int64
	if err := tx.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("surveys/store: list surveys count: %w", err)
	}
	return out, total, nil
}

// buildSurveyListPredicate constructs the parameterised WHERE clause
// for List from the filter. The returned args slice is positional;
// placeholders $1..$N reference args[0..N-1] in order. Limit/Offset are
// NOT included — callers append those at the end of the args slice for
// the SELECT query (the count query uses args as-is).
//
// The tenant predicate is intentionally implicit: List is always run
// inside a Pool.WithTenant transaction, and the surveys_iso RLS policy
// enforces tenant_id = current_setting('app.tenant_id'). No explicit
// tenant filter is added here so the predicate stays minimal.
func buildSurveyListPredicate(f api.ListFilter) (string, []any) {
	args := make([]any, 0, 2)
	predicates := []string{"archived_at IS NULL"}

	if status := f.Status; status != "" {
		args = append(args, string(status))
		predicates = append(predicates, "status = $"+strconv.Itoa(len(args)))
	}
	if trimmed := strings.TrimSpace(f.Search); trimmed != "" {
		args = append(args, "%"+strings.ToLower(trimmed)+"%")
		idx := strconv.Itoa(len(args))
		predicates = append(predicates,
			"(lower(name) LIKE $"+idx+
				" OR lower(description) LIKE $"+idx+")")
	}

	if len(predicates) == 0 {
		return "TRUE", nil
	}
	return strings.Join(predicates, " AND "), args
}

// Update implements api.SurveyStorePort.Update with COALESCE semantics:
// any nil pointer in patch leaves the column untouched, so the SQL
// stays one round-trip regardless of which subset of fields the caller
// wants to change. The WHERE clause excludes archived rows so callers
// cannot accidentally mutate a soft-deleted row; ErrNotFound is
// returned for both the missing and the archived case so the service
// layer can lift it to ErrSurveyArchived after a follow-up Get when it
// needs to discriminate.
//
// updated_at is bumped to now() unconditionally so dashboards see the
// freshly-touched row at the top of any "recently changed" list, even
// when an empty patch reaches the store layer (the service short-
// circuits IsEmpty() before calling, but the DB-level guard stays
// honest).
func (s *SurveyStore) Update(ctx context.Context, tx postgres.Tx, id uuid.UUID, patch api.SurveyPatch) (api.Survey, error) {
	const q = `
		UPDATE surveys SET
			name         = COALESCE($2, name),
			description  = COALESCE($3, description),
			primary_mode = COALESCE($4, primary_mode),
			updated_at   = now()
		WHERE id = $1 AND archived_at IS NULL
		RETURNING ` + surveyColumns

	var modePtr *string
	if patch.PrimaryMode != nil {
		m := string(*patch.PrimaryMode)
		modePtr = &m
	}

	saved, err := scanSurvey(tx.QueryRow(ctx, q,
		id,
		patch.Name,
		patch.Description,
		modePtr,
	))
	if err != nil {
		if terr := translateSurveyErr(err); errors.Is(terr, api.ErrNotFound) {
			return api.Survey{}, terr
		}
		return api.Survey{}, fmt.Errorf("surveys/store: update survey: %w", err)
	}
	return saved, nil
}

// Archive implements api.SurveyStorePort.Archive. Sets status='archived'
// and stamps archived_at to the supplied timestamp. Idempotent: a row
// already archived is left untouched and the call returns nil (no
// ErrNotFound) — the service layer is the right place to reject
// double-archive when business rules demand it.
func (s *SurveyStore) Archive(ctx context.Context, tx postgres.Tx, id uuid.UUID, at time.Time) error {
	const q = `
		UPDATE surveys SET
			status      = 'archived',
			archived_at = COALESCE(archived_at, $2),
			updated_at  = now()
		WHERE id = $1`

	tag, err := tx.Exec(ctx, q, id, at)
	if err != nil {
		return fmt.Errorf("surveys/store: archive survey: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrNotFound
	}
	return nil
}

// SetCurrentVersion implements api.SurveyStorePort.SetCurrentVersion. Used
// by the Activate flow to point surveys.current_version_id at the freshly
// activated version row. Returns ErrNotFound when the survey row is
// absent.
func (s *SurveyStore) SetCurrentVersion(ctx context.Context, tx postgres.Tx, surveyID, versionID uuid.UUID) error {
	const q = `
		UPDATE surveys SET
			current_version_id = $2,
			updated_at         = now()
		WHERE id = $1`

	tag, err := tx.Exec(ctx, q, surveyID, versionID)
	if err != nil {
		return fmt.Errorf("surveys/store: set current version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrNotFound
	}
	return nil
}

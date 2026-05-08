package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// respondentColumns is the canonical projection used by every read
// query so the field order matches scanRespondentRow without drift.
//
// deleted_at is included so the service layer can short-circuit
// operations on already-deleted rows with ErrRespondentDeleted instead
// of a confusing ErrRespondentNotFound. The row stays visible to
// admin tooling for the 30-day grace window before the purge worker
// hard-deletes it.
const respondentColumns = `id, tenant_id, project_id, phone_encrypted, phone_hash,
		region_code, attributes, status, attempts,
		last_attempt_at, next_attempt_at, source, created_at, deleted_at`

// respondentUniqueConstraintCode is the constraint name added by
// 000006_respondents_uniq.up.sql. We match on the explicit name (rather
// than any 23505 on `respondents`) so a future migration that adds a
// second unique index — say, on `external_ref` — surfaces a distinct
// error instead of silently masquerading as ErrDuplicateRespondent.
const respondentUniqueConstraintCode = "respondents_tenant_project_phone_hash_uniq"

// RespondentStore is the Postgres-backed implementation of
// api.RespondentStorePort. Methods delegate to the supplied
// postgres.Tx, so the service layer co-locates the row write with the
// DNC check and the audit row in a single per-tenant transaction.
type RespondentStore struct {
	pool *postgres.Pool
}

// Compile-time assertion that *RespondentStore satisfies the contract.
var _ api.RespondentStorePort = (*RespondentStore)(nil)

// NewRespondentStore constructs a RespondentStore. The pool reference
// is held for symmetry with ProjectStore — current methods all operate
// on the supplied Tx so the pool is unused at every call site. Future
// read-only paths that need an internal BypassRLS tx will use it.
func NewRespondentStore(pool *postgres.Pool) *RespondentStore {
	return &RespondentStore{pool: pool}
}

// scanRespondentRow fills an api.Respondent from a single row.
//
// The function deliberately does NOT populate Phone or PhoneMasked —
// those are derived in the service layer (Phone is decrypted only by
// GetWithPhone; PhoneMasked is a display-time formatter).
func scanRespondentRow(r rowScanner) (api.Respondent, error) {
	var (
		out    api.Respondent
		status string
	)
	err := r.Scan(
		&out.ID,
		&out.TenantID,
		&out.ProjectID,
		&out.PhoneEncrypted,
		&out.PhoneHash,
		&out.RegionCode,
		&out.Attributes,
		&status,
		&out.Attempts,
		&out.LastAttemptAt,
		&out.NextAttemptAt,
		&out.Source,
		&out.CreatedAt,
		&out.DeleteAt,
	)
	if err != nil {
		return api.Respondent{}, err
	}
	out.Status = api.RespondentStatus(status)
	return out, nil
}

// translateRespondentErr maps pgx / pgconn errors into the crm api
// sentinels for the respondents table. pgx.ErrNoRows → ErrRespondentNotFound;
// SQLSTATE 23505 on respondents_tenant_project_phone_hash_uniq →
// ErrDuplicateRespondent. Any other 23505 (different constraint) is
// returned as-is so the caller sees the raw pg error and can decide.
func translateRespondentErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ErrRespondentNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		if pgErr.ConstraintName == respondentUniqueConstraintCode {
			// Wrap the constraint name into the sentinel via
			// errors.Join so callers can errors.Is(err,
			// api.ErrDuplicateRespondent) without losing the
			// diagnostic constraint detail.
			return errors.Join(api.ErrDuplicateRespondent, fmt.Errorf("constraint=%s", pgErr.ConstraintName))
		}
		// Different unique constraint — surface raw error.
	}
	return err
}

// Insert implements api.RespondentStorePort.Insert. The supplied
// Respondent.ID is ignored — Postgres mints a fresh id via
// gen_random_uuid() and the returned row carries the canonical
// id+timestamp. Status defaults to api.RespPending when zero.
func (s *RespondentStore) Insert(ctx context.Context, tx postgres.Tx, r api.Respondent) (api.Respondent, error) {
	const q = `
		INSERT INTO respondents (
			tenant_id, project_id, phone_encrypted, phone_hash,
			region_code, attributes, status, source
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING ` + respondentColumns

	status := r.Status
	if status == "" {
		status = api.RespPending
	}
	source := r.Source
	if source == "" {
		source = api.SourceImported
	}
	attrs := r.Attributes
	if attrs == nil {
		// jsonb NOT NULL DEFAULT '{}' on the DB side, but we set it
		// explicitly so a nil-attribute respondent serialises to {}
		// rather than triggering a NOT NULL violation through pgx's
		// nil→NULL coercion.
		attrs = map[string]any{}
	}

	saved, err := scanRespondentRow(tx.QueryRow(ctx, q,
		r.TenantID,
		r.ProjectID,
		r.PhoneEncrypted,
		r.PhoneHash,
		r.RegionCode,
		attrs,
		string(status),
		source,
	))
	if err != nil {
		// Always run through translateRespondentErr so callers can
		// errors.Is on every recognised sentinel — wrapping the raw
		// err on fall-through could hide a future constraint that
		// translate() learns to map.
		return api.Respondent{}, fmt.Errorf("crm/store: insert respondent: %w", translateRespondentErr(err))
	}
	return saved, nil
}

// GetByID implements api.RespondentStorePort.GetByID inside the
// caller's tx. Returns ErrRespondentNotFound when the row is absent or
// RLS hides it.
func (s *RespondentStore) GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (api.Respondent, error) {
	const q = `SELECT ` + respondentColumns + ` FROM respondents WHERE id = $1`

	out, err := scanRespondentRow(tx.QueryRow(ctx, q, id))
	if err != nil {
		return api.Respondent{}, fmt.Errorf("crm/store: get respondent by id: %w", translateRespondentErr(err))
	}
	return out, nil
}

// GetByHash implements api.RespondentStorePort.GetByHash.
//
// The query is scoped to (tenant_id, project_id, phone_hash) to match
// the respondents_tenant_project_phone_hash_uniq constraint added in
// 000006. Caller-side RLS via Pool.WithTenant adds a second tenant_id
// equality predicate on top, which the planner deduplicates.
func (s *RespondentStore) GetByHash(ctx context.Context, tx postgres.Tx, tenantID, projectID uuid.UUID, phoneHash []byte) (api.Respondent, error) {
	const q = `
		SELECT ` + respondentColumns + `
		FROM respondents
		WHERE tenant_id = $1 AND project_id = $2 AND phone_hash = $3`

	out, err := scanRespondentRow(tx.QueryRow(ctx, q, tenantID, projectID, phoneHash))
	if err != nil {
		return api.Respondent{}, fmt.Errorf("crm/store: get respondent by hash: %w", translateRespondentErr(err))
	}
	return out, nil
}

// IsBlockedDNC implements api.RespondentStorePort.IsBlockedDNC.
//
// Matches both project-scoped entries (project_dnc.project_id =
// projectID) and tenant-wide entries (project_id IS NULL). The
// project_dnc table's unique index on (tenant_id, coalesce(project_id,
// '00000000-0000-0000-0000-000000000000'::uuid), phone_hash) keeps
// either kind unique per phone, but the union check here doesn't
// depend on that index — we just want a boolean "any matching row".
//
// Returns false when no row matches (no error). Returns the raw error
// if the query itself fails.
func (s *RespondentStore) IsBlockedDNC(ctx context.Context, tx postgres.Tx, tenantID, projectID uuid.UUID, phoneHash []byte) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM project_dnc
			WHERE tenant_id = $1
			  AND (project_id = $2 OR project_id IS NULL)
			  AND phone_hash = $3
		)`

	var blocked bool
	if err := tx.QueryRow(ctx, q, tenantID, projectID, phoneHash).Scan(&blocked); err != nil {
		return false, fmt.Errorf("crm/store: is blocked dnc: %w", err)
	}
	return blocked, nil
}

// insertBatchColumns is the column projection used by InsertBatch's
// COPY stream. The order MUST match the row produced by
// CopyFromSlice's callback below; any drift between the two corrupts
// the inserted rows silently.
var insertBatchColumns = []string{
	"tenant_id",
	"project_id",
	"phone_encrypted",
	"phone_hash",
	"region_code",
	"attributes",
	"status",
	"source",
}

// InsertBatch implements api.RespondentStorePort.InsertBatch via
// pgx.CopyFrom. The PostgreSQL COPY protocol streams the rows in a
// single network round-trip and avoids per-row planner work — empirical
// numbers from the pgx README show 10x-100x throughput vs. per-row
// INSERT.
//
// CopyFrom does NOT support ON CONFLICT. The caller MUST run
// ExistingHashes (and dedupe within the batch) BEFORE calling this
// method; a leftover collision rolls back the whole COPY and is
// surfaced as ErrDuplicateRespondent.
//
// Empty input is a no-op that returns (0, nil); CopyFrom on an empty
// source still opens a server-side stream, so the early return is a
// small but real optimisation.
func (s *RespondentStore) InsertBatch(ctx context.Context, tx postgres.Tx, rows []api.Respondent) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		status := r.Status
		if status == "" {
			status = api.RespPending
		}
		source := r.Source
		if source == "" {
			source = api.SourceImported
		}
		attrs := r.Attributes
		if attrs == nil {
			attrs = map[string]any{}
		}
		return []any{
			r.TenantID,
			r.ProjectID,
			r.PhoneEncrypted,
			r.PhoneHash,
			r.RegionCode,
			attrs,
			string(status),
			source,
		}, nil
	})

	count, err := tx.CopyFrom(ctx, pgx.Identifier{"respondents"}, insertBatchColumns, src)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == respondentUniqueConstraintCode {
			return 0, errors.Join(api.ErrDuplicateRespondent, fmt.Errorf("constraint=%s", pgErr.ConstraintName))
		}
		return 0, fmt.Errorf("crm/store: copy from respondents: %w", err)
	}

	// pgx.CopyFrom returns int64; convert with the bounds check the
	// project standards require (07-go-coding-standards.md § Safety).
	if count < 0 {
		return 0, fmt.Errorf("crm/store: copy from respondents: negative row count %d", count)
	}
	if count > int64(maxBatchInsertRows) {
		return 0, fmt.Errorf("crm/store: copy from respondents: row count %d exceeds %d", count, maxBatchInsertRows)
	}
	return int(count), nil
}

// maxBatchInsertRows is the hard cap on rows passed to InsertBatch in
// one call. The import service buffers in 1k-row groups; the cap here
// is defense-in-depth so a misbehaving caller cannot smuggle a
// 100M-row request that would exhaust connection memory.
const maxBatchInsertRows = 100000

// SoftDelete implements api.RespondentStorePort.SoftDelete.
//
// Stamps deleted_at + deletion_reason on the row inside the caller's
// per-tenant transaction. The WHERE clause includes
// `deleted_at IS NULL` so a second SoftDelete against the same id is
// reported as ErrRespondentNotFound (the row exists but is already
// soft-deleted) — the service layer translates that to the more
// specific ErrRespondentDeleted via a follow-up GetByID, which is the
// canonical idempotency check for the public Delete path.
//
// Reason is stored verbatim. The HTTP transport's bind validation caps
// the length so the column never grows past a sensible upper bound.
func (s *RespondentStore) SoftDelete(ctx context.Context, tx postgres.Tx, id uuid.UUID, reason string, at time.Time) error {
	// Flip status to deletion-requested alongside the deleted_at stamp
	// so operator-search by status surfaces these rows during the
	// 30-day grace window. Without this, the RespDeletionRequested
	// constant in api/dto.go is unreachable at the store layer and
	// any Search(filter{status: deletion-requested}) returns nothing.
	const q = `
		UPDATE respondents
		SET deleted_at = $2, deletion_reason = NULLIF($3, ''), status = $4
		WHERE id = $1 AND deleted_at IS NULL`

	tag, err := tx.Exec(ctx, q, id, at, reason, string(api.RespDeletionRequested))
	if err != nil {
		return fmt.Errorf("crm/store: soft-delete respondent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrRespondentNotFound
	}
	return nil
}

// PurgeOlderThan implements api.RespondentStorePort.PurgeOlderThan.
//
// Hard-deletes up to `limit` rows whose deleted_at < cutoff and
// returns the affected ids. The DELETE ... LIMIT clause uses a CTE
// because PostgreSQL does not accept LIMIT directly on DELETE; the
// CTE picks the candidate ids first (with `FOR UPDATE SKIP LOCKED`
// so multiple purger replicas can run concurrently without
// double-counting) and the outer DELETE removes them.
//
// Empty result returns (nil, nil); a non-positive limit returns (nil, nil)
// without running a query.
func (s *RespondentStore) PurgeOlderThan(ctx context.Context, tx postgres.Tx, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		return nil, nil
	}

	const q = `
		WITH candidates AS (
			SELECT id FROM respondents
			WHERE deleted_at IS NOT NULL AND deleted_at < $1
			ORDER BY deleted_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM respondents
		WHERE id IN (SELECT id FROM candidates)
		RETURNING id`

	rows, err := tx.Query(ctx, q, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("crm/store: purge respondents: %w", err)
	}
	defer rows.Close()

	out := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, fmt.Errorf("crm/store: purge respondents scan: %w", scanErr)
		}
		out = append(out, id)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("crm/store: purge respondents iterate: %w", rerr)
	}
	return out, nil
}

// Search implements api.RespondentStorePort.Search.
//
// Builds a parameterised WHERE clause from the filter, runs one paged
// SELECT and one COUNT(*) query, returns the materialised slice plus
// the total count. The query filters out soft-deleted rows
// (deleted_at IS NULL) — admin tooling that needs to surface
// pending-purge respondents uses a dedicated path (not Search).
//
// Sort order is created_at DESC, id DESC so newest rows surface first
// and the secondary id key keeps pagination stable when many rows
// share the same created_at.
func (s *RespondentStore) Search(ctx context.Context, tx postgres.Tx, f api.SearchRespondentsFilter) ([]api.Respondent, int64, error) {
	clause, args := buildSearchPredicate(f)

	limit := f.PageSize
	if limit <= 0 {
		limit = 50
	}
	offset := 0
	if f.Page > 1 {
		offset = (f.Page - 1) * limit
	}

	listQ := `
		SELECT ` + respondentColumns + `
		FROM respondents
		WHERE ` + clause + `
		ORDER BY created_at DESC, id DESC
		LIMIT $` + intArg(len(args)+1) + ` OFFSET $` + intArg(len(args)+2)

	countQ := `
		SELECT count(*)
		FROM respondents
		WHERE ` + clause

	listArgs := append(append([]any{}, args...), limit, offset)

	rows, err := tx.Query(ctx, listQ, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("crm/store: search respondents query: %w", err)
	}
	defer rows.Close()

	out := make([]api.Respondent, 0)
	for rows.Next() {
		r, scanErr := scanRespondentRow(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("crm/store: search respondents scan: %w", scanErr)
		}
		out = append(out, r)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, 0, fmt.Errorf("crm/store: search respondents iterate: %w", rerr)
	}

	var total int64
	if cerr := tx.QueryRow(ctx, countQ, args...).Scan(&total); cerr != nil {
		return nil, 0, fmt.Errorf("crm/store: search respondents count: %w", cerr)
	}
	return out, total, nil
}

// buildSearchPredicate constructs the parameterised WHERE clause for
// Search from the filter. The returned args slice is positional;
// placeholders $1..$N reference args[0..N-1] in order. Limit/Offset
// are NOT included — the caller appends those at the end of the args
// slice for the SELECT query.
func buildSearchPredicate(f api.SearchRespondentsFilter) (string, []any) {
	args := []any{f.TenantID, f.ProjectID}
	predicates := []string{
		"tenant_id = $1",
		"project_id = $2",
		"deleted_at IS NULL",
	}

	if f.Status != nil {
		args = append(args, string(*f.Status))
		predicates = append(predicates, "status = $"+intArg(len(args)))
	}
	if region := strings.TrimSpace(f.Region); region != "" {
		args = append(args, region)
		predicates = append(predicates, "region_code = $"+intArg(len(args)))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		// Match against the JSON attributes blob. We cast to text and
		// use ILIKE so the query stays portable without the pg_trgm
		// extension (which not every test fixture has installed). The
		// cost is one full scan per page; for v1's expected scale (a
		// few thousand respondents per project) this is fine.
		args = append(args, "%"+strings.ToLower(q)+"%")
		predicates = append(predicates,
			"(lower(attributes::text) LIKE $"+intArg(len(args))+
				" OR lower(region_code) LIKE $"+intArg(len(args))+")")
	}
	return strings.Join(predicates, " AND "), args
}

// ExistingHashes implements api.RespondentStorePort.ExistingHashes.
// Used by the import path to filter out rows whose phone_hash is
// already present in (tenant_id, project_id) BEFORE the COPY runs —
// CopyFrom doesn't support ON CONFLICT and a single collision rolls
// back the whole batch.
//
// Empty input returns (nil, nil) without a query.
func (s *RespondentStore) ExistingHashes(ctx context.Context, tx postgres.Tx, tenantID, projectID uuid.UUID, hashes [][]byte) ([][]byte, error) {
	if len(hashes) == 0 {
		return nil, nil
	}

	const q = `
		SELECT phone_hash
		FROM respondents
		WHERE tenant_id = $1
		  AND project_id = $2
		  AND phone_hash = ANY($3::bytea[])`

	rows, err := tx.Query(ctx, q, tenantID, projectID, hashes)
	if err != nil {
		return nil, fmt.Errorf("crm/store: existing hashes query: %w", err)
	}
	defer rows.Close()

	out := make([][]byte, 0, len(hashes))
	for rows.Next() {
		var h []byte
		if scanErr := rows.Scan(&h); scanErr != nil {
			return nil, fmt.Errorf("crm/store: existing hashes scan: %w", scanErr)
		}
		out = append(out, h)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("crm/store: existing hashes rows: %w", rerr)
	}
	return out, nil
}

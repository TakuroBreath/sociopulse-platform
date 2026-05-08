package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// respondentColumns is the canonical projection used by every read
// query so the field order matches scanRespondentRow without drift.
const respondentColumns = `id, tenant_id, project_id, phone_encrypted, phone_hash,
		region_code, attributes, status, attempts,
		last_attempt_at, next_attempt_at, source, created_at`

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
		if terr := translateRespondentErr(err); errors.Is(terr, api.ErrDuplicateRespondent) {
			return api.Respondent{}, terr
		}
		return api.Respondent{}, fmt.Errorf("crm/store: insert respondent: %w", err)
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
		if terr := translateRespondentErr(err); errors.Is(terr, api.ErrRespondentNotFound) {
			return api.Respondent{}, terr
		}
		return api.Respondent{}, fmt.Errorf("crm/store: get respondent by id: %w", err)
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
		if terr := translateRespondentErr(err); errors.Is(terr, api.ErrRespondentNotFound) {
			return api.Respondent{}, terr
		}
		return api.Respondent{}, fmt.Errorf("crm/store: get respondent by hash: %w", err)
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

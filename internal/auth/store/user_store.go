package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// pgUniqueViolation is the SQLSTATE code Postgres returns for a unique-
// constraint violation. Translated to api.ErrLoginTaken for the
// (tenant_id, login) uniqueness invariant on the users table.
const pgUniqueViolation = "23505"

// UserStore is the Postgres-backed implementation of api.UserStorePort.
//
// Mutating methods accept a postgres.Tx so the auth service layer can
// co-locate the row write with audit and outbox writes in the same
// transaction. Read methods take the same Tx — the service is expected
// to open a per-tenant transaction (Pool.WithTenant) and chain every
// store call through it so the RLS policy applies uniformly.
//
// Cross-module callers MUST import from internal/auth/api only;
// depguard's module-boundaries rule rejects direct imports of this
// package from outside the auth module.
type UserStore struct {
	pool *postgres.Pool
}

// Compile-time assertion that *UserStore satisfies api.UserStorePort.
var _ api.UserStorePort = (*UserStore)(nil)

// NewUserStore constructs a UserStore. The pool reference is held for
// symmetry with internal/tenancy/store — the current methods all
// operate on the supplied Tx, so the pool is unused at every call
// site. Future read paths that need an internal BypassRLS tx will use
// it.
func NewUserStore(pool *postgres.Pool) *UserStore {
	return &UserStore{pool: pool}
}

// rolesToStrings converts an api.Role slice into a []string suitable
// for pgx text[] argument binding.
func rolesToStrings(roles []api.Role) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}

// stringsToRoles converts a []string scanned from a text[] column into
// the api.Role enum slice. Unknown values pass through verbatim; the
// users_roles_valid CHECK constraint and service-layer validation
// guard the allowed set.
func stringsToRoles(in []string) []api.Role {
	out := make([]api.Role, len(in))
	for i, s := range in {
		out[i] = api.Role(s)
	}
	return out
}

// userColumns is the canonical projection used by every read query so
// the field order matches scanRow without drift across call sites.
const userColumns = `id, tenant_id, login, full_name, email, roles,
	totp_enabled, must_change_pwd, created_at, updated_at, archived_at`

// rowScanner abstracts pgx.Row and a single pgx.Rows step so scanRow can
// be reused across QueryRow and rows.Next loops.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRow fills an api.User from a single row, normalising text[] roles
// and the nullable archived_at timestamp.
func scanRow(r rowScanner) (api.User, error) {
	var (
		u        api.User
		rolesRaw []string
	)
	err := r.Scan(
		&u.ID, &u.TenantID, &u.Login, &u.FullName, &u.Email, &rolesRaw,
		&u.TOTPEnabled, &u.MustChangePwd, &u.CreatedAt, &u.UpdatedAt, &u.ArchivedAt,
	)
	if err != nil {
		return api.User{}, err
	}
	u.Roles = stringsToRoles(rolesRaw)
	return u, nil
}

// translateErr maps pgx / pgconn errors into the auth api sentinels.
// pgx.ErrNoRows -> ErrUserNotFound; SQLSTATE 23505 (unique violation) on
// any users_*_login uniqueness index -> ErrLoginTaken.
func translateErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ErrUserNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return fmt.Errorf("%w: %s", api.ErrLoginTaken, pgErr.ConstraintName)
	}
	return err
}

// GetByID implements api.UserStorePort.GetByID inside the caller's tx.
func (s *UserStore) GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (api.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = $1`

	u, err := scanRow(tx.QueryRow(ctx, q, id))
	if err != nil {
		if terr := translateErr(err); errors.Is(terr, api.ErrUserNotFound) {
			return api.User{}, terr
		}
		return api.User{}, fmt.Errorf("auth/store: get user by id: %w", err)
	}
	return u, nil
}

// GetByLogin implements api.UserStorePort.GetByLogin inside the caller's tx.
// Lookup is case-insensitive: the partial index idx_users_lower_login covers
// the (tenant_id, lower(login)) predicate.
func (s *UserStore) GetByLogin(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, login string) (api.User, error) {
	const q = `
		SELECT ` + userColumns + `
		FROM users
		WHERE tenant_id = $1 AND lower(login) = lower($2)`

	u, err := scanRow(tx.QueryRow(ctx, q, tenantID, login))
	if err != nil {
		if terr := translateErr(err); errors.Is(terr, api.ErrUserNotFound) {
			return api.User{}, terr
		}
		return api.User{}, fmt.Errorf("auth/store: get user by login: %w", err)
	}
	return u, nil
}

// List implements api.UserStorePort.List inside the caller's tx. The
// total count comes from a second query so the result is the unfiltered
// row count for the tenant filter (used to drive admin pagination
// counters). Both queries respect the same archived-or-not predicate.
func (s *UserStore) List(ctx context.Context, tx postgres.Tx, in api.ListUsersInput) ([]api.User, int64, error) {
	const listQ = `
		SELECT ` + userColumns + `
		FROM users
		WHERE tenant_id = $1
		  AND ($2::boolean OR archived_at IS NULL)
		ORDER BY login
		LIMIT $3 OFFSET $4`

	const countQ = `
		SELECT count(*)
		FROM users
		WHERE tenant_id = $1
		  AND ($2::boolean OR archived_at IS NULL)`

	rows, err := tx.Query(ctx, listQ, in.TenantID, in.IncludeArchived, in.Limit, in.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("auth/store: list users query: %w", err)
	}
	defer rows.Close()

	out := make([]api.User, 0)
	for rows.Next() {
		u, err := scanRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("auth/store: list users scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("auth/store: list users iterate: %w", err)
	}

	var total int64
	if err := tx.QueryRow(ctx, countQ, in.TenantID, in.IncludeArchived).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("auth/store: list users count: %w", err)
	}
	return out, total, nil
}

// Insert implements api.UserStorePort.Insert. The supplied User.ID is
// ignored — Postgres mints a fresh id via gen_random_uuid() and the
// returned row carries the canonical id+timestamps. created_by is set
// to NULL: the api.User DTO does not carry an actor, and the audit row
// owns the who-created-this question instead. A future migration can
// repopulate created_by from the audit trail if needed.
func (s *UserStore) Insert(ctx context.Context, tx postgres.Tx, u api.User, hash string) (api.User, error) {
	const q = `
		INSERT INTO users (
			tenant_id, login, full_name, email, password_hash,
			must_change_pwd, roles, totp_enabled, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING ` + userColumns

	saved, err := scanRow(tx.QueryRow(ctx, q,
		u.TenantID,
		u.Login,
		u.FullName,
		u.Email,
		hash,
		u.MustChangePwd,
		rolesToStrings(u.Roles),
		u.TOTPEnabled,
		nil, // created_by — audit row owns this
	))
	if err != nil {
		if terr := translateErr(err); errors.Is(terr, api.ErrLoginTaken) {
			return api.User{}, terr
		}
		return api.User{}, fmt.Errorf("auth/store: insert user: %w", err)
	}
	return saved, nil
}

// UpdateRoles implements api.UserStorePort.UpdateRoles. The CHECK
// constraint users_roles_nonempty rejects an empty array — the service
// layer also guards this, but the DB invariant is the last word.
func (s *UserStore) UpdateRoles(ctx context.Context, tx postgres.Tx, id uuid.UUID, roles []api.Role) (api.User, error) {
	const q = `
		UPDATE users
		SET roles = $2, updated_at = now()
		WHERE id = $1
		RETURNING ` + userColumns

	u, err := scanRow(tx.QueryRow(ctx, q, id, rolesToStrings(roles)))
	if err != nil {
		if terr := translateErr(err); errors.Is(terr, api.ErrUserNotFound) {
			return api.User{}, terr
		}
		return api.User{}, fmt.Errorf("auth/store: update roles: %w", err)
	}
	return u, nil
}

// UpdatePassword implements api.UserStorePort.UpdatePassword.
func (s *UserStore) UpdatePassword(ctx context.Context, tx postgres.Tx, id uuid.UUID, hash string, mustChange bool) error {
	const q = `
		UPDATE users
		SET password_hash = $2, must_change_pwd = $3, updated_at = now()
		WHERE id = $1`

	tag, err := tx.Exec(ctx, q, id, hash, mustChange)
	if err != nil {
		return fmt.Errorf("auth/store: update password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrUserNotFound
	}
	return nil
}

// Archive implements api.UserStorePort.Archive. Idempotent: archiving an
// already-archived user is a no-op (rows-affected is 0 on the partial
// predicate, but the row exists, so we distinguish that from the
// genuine "no such id" case via a follow-up existence check).
func (s *UserStore) Archive(ctx context.Context, tx postgres.Tx, id uuid.UUID) error {
	const q = `
		UPDATE users
		SET archived_at = now(), updated_at = now()
		WHERE id = $1 AND archived_at IS NULL`

	tag, err := tx.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("auth/store: archive user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the row is missing, or it is already archived. Distinguish
		// so the service layer can surface ErrUserNotFound for the missing
		// case while staying silent on the idempotent re-archive.
		if err := s.checkUserExists(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// Restore implements api.UserStorePort.Restore. Returns ErrUserNotArchived
// when the user exists but is not currently archived so the service layer
// can refuse the request loudly.
func (s *UserStore) Restore(ctx context.Context, tx postgres.Tx, id uuid.UUID) error {
	const q = `
		UPDATE users
		SET archived_at = NULL, updated_at = now()
		WHERE id = $1 AND archived_at IS NOT NULL`

	tag, err := tx.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("auth/store: restore user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the row is missing or the user is not archived. Resolve
		// the ambiguity so the service layer returns the right sentinel.
		if err := s.checkUserExists(ctx, tx, id); err != nil {
			return err
		}
		return api.ErrUserNotArchived
	}
	return nil
}

// SetTOTPEnabled implements api.UserStorePort.SetTOTPEnabled.
func (s *UserStore) SetTOTPEnabled(ctx context.Context, tx postgres.Tx, id uuid.UUID, enabled bool) error {
	const q = `
		UPDATE users
		SET totp_enabled = $2, updated_at = now()
		WHERE id = $1`

	tag, err := tx.Exec(ctx, q, id, enabled)
	if err != nil {
		return fmt.Errorf("auth/store: set totp enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrUserNotFound
	}
	return nil
}

// GetPasswordHash implements api.UserStorePort.GetPasswordHash. Returns
// the raw PHC-encoded Argon2id hash for password verification.
func (s *UserStore) GetPasswordHash(ctx context.Context, tx postgres.Tx, id uuid.UUID) (string, error) {
	const q = `SELECT password_hash FROM users WHERE id = $1`

	var hash string
	if err := tx.QueryRow(ctx, q, id).Scan(&hash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", api.ErrUserNotFound
		}
		return "", fmt.Errorf("auth/store: get password hash: %w", err)
	}
	return hash, nil
}

// checkUserExists returns ErrUserNotFound when the row is absent. Used
// by Archive/Restore to disambiguate the "0 rows updated" outcome.
func (s *UserStore) checkUserExists(ctx context.Context, tx postgres.Tx, id uuid.UUID) error {
	const q = `SELECT 1 FROM users WHERE id = $1`
	var dummy int
	if err := tx.QueryRow(ctx, q, id).Scan(&dummy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return api.ErrUserNotFound
		}
		return fmt.Errorf("auth/store: check user exists: %w", err)
	}
	return nil
}

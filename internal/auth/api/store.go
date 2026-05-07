package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// UserStorePort is the persistence contract the auth UserService consumes.
// It lives in api/ (not in store/) so the service package depends only on
// abstractions and tests can supply a hand-rolled fake without crossing the
// depguard module boundary.
//
// All mutating methods accept a postgres.Tx so the service can co-locate
// the row write with audit and outbox writes inside one transaction.
// Read methods also accept a Tx for symmetry — the service typically opens
// a per-tenant transaction (pool.WithTenant) and chains the read through it
// so the same RLS policy applies.
type UserStorePort interface {
	// GetByID returns the user with the supplied id (regardless of archive
	// state). Returns ErrUserNotFound when the row is absent or RLS hides it.
	GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (User, error)

	// GetByLogin returns the user matching (tenantID, lower(login)).
	// Returns ErrUserNotFound when missing.
	GetByLogin(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, login string) (User, error)

	// List returns one page of users plus the unfiltered total count for the
	// tenant filter. Pagination clamping is the service layer's
	// responsibility; the store treats Limit/Offset as authoritative.
	List(ctx context.Context, tx postgres.Tx, in ListUsersInput) (rows []User, total int64, err error)

	// Insert creates a new user. The supplied User.ID may be uuid.Nil — the
	// store relies on the column DEFAULT to mint a fresh id and returns the
	// row populated with id, created_at, updated_at. The hash arg is the
	// PHC-encoded Argon2id hash; the store stores it verbatim.
	// Returns ErrLoginTaken on (tenant_id, login) unique violation.
	Insert(ctx context.Context, tx postgres.Tx, u User, hash string) (User, error)

	// UpdateRoles replaces the user's roles atomically and bumps updated_at.
	// Returns the refreshed row. ErrUserNotFound when id missing.
	UpdateRoles(ctx context.Context, tx postgres.Tx, id uuid.UUID, roles []Role) (User, error)

	// UpdatePassword swaps the password hash and the must-change-password
	// flag in one statement, bumping updated_at. ErrUserNotFound when id
	// missing.
	UpdatePassword(ctx context.Context, tx postgres.Tx, id uuid.UUID, hash string, mustChange bool) error

	// Archive sets archived_at = now() if currently NULL. Idempotent —
	// archiving an already-archived user returns nil without changing the
	// timestamp. ErrUserNotFound when id missing.
	Archive(ctx context.Context, tx postgres.Tx, id uuid.UUID) error

	// Restore clears archived_at if currently non-NULL. Returns
	// ErrUserNotArchived when archived_at is already NULL so the service
	// layer can surface the right sentinel. ErrUserNotFound when id missing.
	Restore(ctx context.Context, tx postgres.Tx, id uuid.UUID) error

	// SetTOTPEnabled flips the totp_enabled flag and bumps updated_at.
	// Plan 06 (TOTPService) drives this; UserService exposes it via the
	// admin reset path. ErrUserNotFound when id missing.
	SetTOTPEnabled(ctx context.Context, tx postgres.Tx, id uuid.UUID, enabled bool) error

	// GetPasswordHash returns the password_hash column for the user. Used
	// by UserService.ChangePassword to verify the old password before
	// rotating. ErrUserNotFound when id missing.
	GetPasswordHash(ctx context.Context, tx postgres.Tx, id uuid.UUID) (hash string, err error)
}

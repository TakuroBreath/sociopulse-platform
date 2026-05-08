package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TOTPStore is the Postgres-backed adapter for the auth_totp table.
// Mutating methods accept a postgres.Tx so the auth/service TOTPService
// can co-locate row writes with audit emission inside one tenant-scoped
// transaction (Pool.WithTenant). Reads also accept a Tx so the same RLS
// policy applies — every TOTP row carries a tenant_id and the table's
// auth_totp_tenant_isolation policy enforces the SET LOCAL app.tenant_id
// path uniformly.
//
// Cross-module callers MUST go through internal/auth/api;
// depguard's module-boundaries rule rejects direct imports of this
// package from outside the auth module. Inside auth/, the service layer
// holds a *TOTPStore directly and binds it to the consumer-side
// TOTPStorePort interface (defined in internal/auth/service/totp.go).
type TOTPStore struct {
	pool *postgres.Pool
}

// NewTOTPStore constructs a TOTPStore. The pool is held for symmetry
// with UserStore and is currently unused — every method operates on the
// supplied Tx — but a future internal "self-tenant" lookup path may
// need it. Constructor returns a concrete type per project convention
// (07-go-coding-standards § Interfaces).
func NewTOTPStore(pool *postgres.Pool) *TOTPStore {
	return &TOTPStore{pool: pool}
}

// totpColumns is the canonical projection used by every read so the
// field order matches scanTOTPRow without drift.
const totpColumns = `user_id, tenant_id, secret_enc, enrolled, enrolled_at,
		last_verified_at, backup_codes_hash, backup_used_count`

// scanTOTPRow fills an api.TOTPState from a single row, normalising
// nullable timestamps and the text[] backup hashes.
func scanTOTPRow(r rowScanner) (api.TOTPState, error) {
	var (
		s        api.TOTPState
		hashes   []string
		enrAt    *time.Time
		lastVer  *time.Time
		usedCnt  int
		secret   []byte
		userID   uuid.UUID
		tenantID uuid.UUID
		enrolled bool
	)
	if err := r.Scan(
		&userID, &tenantID, &secret, &enrolled, &enrAt,
		&lastVer, &hashes, &usedCnt,
	); err != nil {
		return api.TOTPState{}, err
	}
	s.UserID = userID
	s.TenantID = tenantID
	s.SecretEncrypted = secret
	s.Enrolled = enrolled
	s.EnrolledAt = enrAt
	s.LastVerifiedAt = lastVer
	s.BackupCodeHashes = hashes
	s.BackupUsedCount = usedCnt
	return s, nil
}

// Upsert writes (or overwrites) the encrypted secret + backup hashes
// for userID. If a row already exists it is reset to the partial-
// enrollment shape: enrolled=false, enrolled_at=NULL, last_verified_at
// preserved nullable, backup_used_count=0. This makes "Enroll twice
// before Confirm" a re-issue of the secret — the documented Plan 05
// Task 6 pragmatic decision (no partial-row carryover).
func (s *TOTPStore) Upsert(
	ctx context.Context,
	tx postgres.Tx,
	userID, tenantID uuid.UUID,
	encSecret []byte,
	backupHashes []string,
) error {
	const q = `
		INSERT INTO auth_totp (
			user_id, tenant_id, secret_enc, enrolled,
			enrolled_at, last_verified_at,
			backup_codes_hash, backup_used_count, created_at, updated_at
		) VALUES ($1, $2, $3, false, NULL, NULL, $4, 0, now(), now())
		ON CONFLICT (user_id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			secret_enc = EXCLUDED.secret_enc,
			enrolled = false,
			enrolled_at = NULL,
			backup_codes_hash = EXCLUDED.backup_codes_hash,
			backup_used_count = 0,
			updated_at = now()`

	if _, err := tx.Exec(ctx, q, userID, tenantID, encSecret, backupHashes); err != nil {
		return fmt.Errorf("auth/store: upsert totp: %w", err)
	}
	return nil
}

// Get fetches the row. Returns api.ErrTOTPNotEnrolled when the row is
// absent OR when the row exists but enrolled=false — for Verify
// purposes a partial-enrollment row is "not enrolled". Confirm and
// Status need to inspect the underlying state directly (separate
// helpers below).
func (s *TOTPStore) Get(ctx context.Context, tx postgres.Tx, userID uuid.UUID) (api.TOTPState, error) {
	state, err := s.GetAny(ctx, tx, userID)
	if err != nil {
		return api.TOTPState{}, err
	}
	if !state.Enrolled {
		return api.TOTPState{}, api.ErrTOTPNotEnrolled
	}
	return state, nil
}

// GetAny fetches the row without filtering on enrolled — used by
// Confirm (which needs to read the partial row) and Status (which
// reports enrolled=false explicitly). Returns api.ErrTOTPNotEnrolled
// when the row is absent (no carryover from prior enrolment attempts).
func (s *TOTPStore) GetAny(ctx context.Context, tx postgres.Tx, userID uuid.UUID) (api.TOTPState, error) {
	const q = `SELECT ` + totpColumns + ` FROM auth_totp WHERE user_id = $1`

	state, err := scanTOTPRow(tx.QueryRow(ctx, q, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return api.TOTPState{}, api.ErrTOTPNotEnrolled
		}
		return api.TOTPState{}, fmt.Errorf("auth/store: get totp: %w", err)
	}
	return state, nil
}

// Confirm flips enrolled=true and stamps enrolled_at. Returns
// api.ErrTOTPNotEnrolled when there is no row to confirm (caller forgot
// Enroll first). Idempotent on an already-enrolled row: the UPDATE
// matches but the DB-side now() advances enrolled_at, so callers that
// care about the original timestamp must read GetAny first; the service
// layer treats double-Confirm as a no-op upstream.
func (s *TOTPStore) Confirm(ctx context.Context, tx postgres.Tx, userID uuid.UUID, at time.Time) error {
	const q = `
		UPDATE auth_totp
		SET enrolled = true, enrolled_at = $2, updated_at = $2
		WHERE user_id = $1`

	tag, err := tx.Exec(ctx, q, userID, at)
	if err != nil {
		return fmt.Errorf("auth/store: confirm totp: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrTOTPNotEnrolled
	}
	return nil
}

// Delete removes the row outright. Disable is documented as idempotent
// — calling on a non-existent row is not an error. The cascade on
// users.id ON DELETE CASCADE keeps the row in lockstep with user
// archival.
func (s *TOTPStore) Delete(ctx context.Context, tx postgres.Tx, userID uuid.UUID) error {
	const q = `DELETE FROM auth_totp WHERE user_id = $1`

	if _, err := tx.Exec(ctx, q, userID); err != nil {
		return fmt.Errorf("auth/store: delete totp: %w", err)
	}
	return nil
}

// UpdateLastVerified stamps last_verified_at on a successful Verify.
// Returns api.ErrTOTPNotEnrolled when the row is absent so a stale
// caller cannot update a deleted row silently.
func (s *TOTPStore) UpdateLastVerified(ctx context.Context, tx postgres.Tx, userID uuid.UUID, at time.Time) error {
	const q = `
		UPDATE auth_totp
		SET last_verified_at = $2, updated_at = $2
		WHERE user_id = $1`

	tag, err := tx.Exec(ctx, q, userID, at)
	if err != nil {
		return fmt.Errorf("auth/store: update totp last verified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrTOTPNotEnrolled
	}
	return nil
}

// MarkBackupUsed atomically removes one matching backup-hash from the
// backup_codes_hash array and bumps backup_used_count. The DB-side
// array_remove(array, value) operates on the entire row in one
// statement so a concurrent verify against the same hash cannot
// double-spend it: the second UPDATE sees an array that no longer
// contains hashToRemove and the count guard (cardinality check below)
// rejects the no-op as ErrTOTPInvalid at the service layer.
//
// Returns api.ErrTOTPNotEnrolled when the row is absent. Returns
// api.ErrTOTPInvalid when the hash was not present (already used or
// never issued).
func (s *TOTPStore) MarkBackupUsed(ctx context.Context, tx postgres.Tx, userID uuid.UUID, hashToRemove string) error {
	const q = `
		UPDATE auth_totp
		SET backup_codes_hash = array_remove(backup_codes_hash, $2),
		    backup_used_count = backup_used_count + 1,
		    updated_at = now()
		WHERE user_id = $1
		  AND $2 = ANY(backup_codes_hash)`

	tag, err := tx.Exec(ctx, q, userID, hashToRemove)
	if err != nil {
		return fmt.Errorf("auth/store: mark backup used: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the row is missing, or the hash is not in the array.
		// Disambiguate so the service layer can route correctly.
		if exists, exErr := s.exists(ctx, tx, userID); exErr != nil {
			return exErr
		} else if !exists {
			return api.ErrTOTPNotEnrolled
		}
		return api.ErrTOTPInvalid
	}
	return nil
}

// exists reports whether an auth_totp row exists for userID.
func (s *TOTPStore) exists(ctx context.Context, tx postgres.Tx, userID uuid.UUID) (bool, error) {
	const q = `SELECT 1 FROM auth_totp WHERE user_id = $1`
	var dummy int
	if err := tx.QueryRow(ctx, q, userID).Scan(&dummy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("auth/store: check totp exists: %w", err)
	}
	return true, nil
}

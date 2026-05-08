//go:build integration

// Integration tests for auth/store.TOTPStore against a real Postgres 16
// instance booted via testcontainers-go. The tests apply the project's
// full migration set (000001 + 000002 + 000003 + 000004), then exercise
// the store via *postgres.Pool.WithTenant so the auth_totp_tenant_isolation
// RLS policy is in effect for every read/write.
//
// Run: go test -tags=integration -count=1 -timeout 5m ./internal/auth/store/...

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/internal/auth/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// seedUser inserts a user inside a per-tenant tx so the auth_totp row
// has a real users.id FK target.
func seedUser(t *testing.T, ctx context.Context, pool *postgres.Pool, tenantID uuid.UUID, login string) uuid.UUID {
	t.Helper()
	us := store.NewUserStore(pool)
	var u authapi.User
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		u, err = us.Insert(ctx, tx, authapi.User{
			TenantID: tenantID,
			Login:    login,
			FullName: login,
			Roles:    []authapi.Role{authapi.RoleOperator},
		}, "argon2id$placeholder")
		return err
	}))
	return u.ID
}

func TestTOTPStore_Upsert_Get_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-TOTP-1")
	userID := seedUser(t, ctx, pool, tenantID, "alice")
	s := store.NewTOTPStore(pool)

	enc := []byte("\x01\x02\x03ciphertext")
	hashes := []string{"hash1", "hash2", "hash3"}
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Upsert(ctx, tx, userID, tenantID, enc, hashes)
	}))

	// GetAny returns the partial-enrollment row.
	var state authapi.TOTPState
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		state, err = s.GetAny(ctx, tx, userID)
		return err
	}))
	require.Equal(t, userID, state.UserID)
	require.Equal(t, tenantID, state.TenantID)
	require.Equal(t, enc, state.SecretEncrypted)
	require.False(t, state.Enrolled)
	require.Nil(t, state.EnrolledAt)
	require.ElementsMatch(t, hashes, state.BackupCodeHashes)
	require.Zero(t, state.BackupUsedCount)

	// Get rejects a partial row as ErrTOTPNotEnrolled.
	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.Get(ctx, tx, userID)
		return err
	})
	require.ErrorIs(t, err, authapi.ErrTOTPNotEnrolled)
}

func TestTOTPStore_Upsert_OverwritesExisting(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-TOTP-OW")
	userID := seedUser(t, ctx, pool, tenantID, "alice")
	s := store.NewTOTPStore(pool)

	first := []byte("first-secret")
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Upsert(ctx, tx, userID, tenantID, first, []string{"h1"})
	}))

	// Confirm to set enrolled=true so we can prove Upsert resets it.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Confirm(ctx, tx, userID, time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	}))

	second := []byte("second-secret")
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Upsert(ctx, tx, userID, tenantID, second, []string{"h2", "h3"})
	}))

	var state authapi.TOTPState
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		state, err = s.GetAny(ctx, tx, userID)
		return err
	}))
	require.Equal(t, second, state.SecretEncrypted)
	require.False(t, state.Enrolled, "Upsert must reset enrolled to false")
	require.Nil(t, state.EnrolledAt)
	require.ElementsMatch(t, []string{"h2", "h3"}, state.BackupCodeHashes)
}

func TestTOTPStore_Confirm_FlipsEnrolledAndStamps(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-TOTP-CONF")
	userID := seedUser(t, ctx, pool, tenantID, "alice")
	s := store.NewTOTPStore(pool)

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Upsert(ctx, tx, userID, tenantID, []byte("enc"), []string{"h"})
	}))

	at := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Confirm(ctx, tx, userID, at)
	}))

	var state authapi.TOTPState
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		state, err = s.Get(ctx, tx, userID)
		return err
	}))
	require.True(t, state.Enrolled)
	require.NotNil(t, state.EnrolledAt)
	require.True(t, state.EnrolledAt.Equal(at))
}

func TestTOTPStore_Confirm_ReturnsErrWhenAbsent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-TOTP-CONF-MISS")
	userID := seedUser(t, ctx, pool, tenantID, "alice")
	s := store.NewTOTPStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Confirm(ctx, tx, userID, time.Now())
	})
	require.ErrorIs(t, err, authapi.ErrTOTPNotEnrolled)
}

func TestTOTPStore_Delete_RemovesRow(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-TOTP-DEL")
	userID := seedUser(t, ctx, pool, tenantID, "alice")
	s := store.NewTOTPStore(pool)

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Upsert(ctx, tx, userID, tenantID, []byte("enc"), []string{"h"})
	}))

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Delete(ctx, tx, userID)
	}))

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.GetAny(ctx, tx, userID)
		return err
	})
	require.ErrorIs(t, err, authapi.ErrTOTPNotEnrolled)

	// Idempotent: deleting again is a no-op.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Delete(ctx, tx, userID)
	}))
}

func TestTOTPStore_UpdateLastVerified_StampsTimestamp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-TOTP-LV")
	userID := seedUser(t, ctx, pool, tenantID, "alice")
	s := store.NewTOTPStore(pool)

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if err := s.Upsert(ctx, tx, userID, tenantID, []byte("enc"), []string{"h"}); err != nil {
			return err
		}
		return s.Confirm(ctx, tx, userID, time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	}))

	at := time.Date(2026, 5, 8, 13, 30, 0, 0, time.UTC)
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.UpdateLastVerified(ctx, tx, userID, at)
	}))

	var state authapi.TOTPState
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		state, err = s.Get(ctx, tx, userID)
		return err
	}))
	require.NotNil(t, state.LastVerifiedAt)
	require.True(t, state.LastVerifiedAt.Equal(at))
}

func TestTOTPStore_MarkBackupUsed_RemovesAndIncrements(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-TOTP-BU")
	userID := seedUser(t, ctx, pool, tenantID, "alice")
	s := store.NewTOTPStore(pool)

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if err := s.Upsert(ctx, tx, userID, tenantID, []byte("enc"), []string{"h-a", "h-b", "h-c"}); err != nil {
			return err
		}
		return s.Confirm(ctx, tx, userID, time.Now())
	}))

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.MarkBackupUsed(ctx, tx, userID, "h-b")
	}))

	var state authapi.TOTPState
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		state, err = s.Get(ctx, tx, userID)
		return err
	}))
	require.ElementsMatch(t, []string{"h-a", "h-c"}, state.BackupCodeHashes)
	require.Equal(t, 1, state.BackupUsedCount)

	// Re-using the same hash returns ErrTOTPInvalid (single-use guarantee).
	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.MarkBackupUsed(ctx, tx, userID, "h-b")
	})
	require.ErrorIs(t, err, authapi.ErrTOTPInvalid)
}

func TestTOTPStore_MarkBackupUsed_AbsentRow(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-TOTP-BU-MISS")
	userID := seedUser(t, ctx, pool, tenantID, "alice")
	s := store.NewTOTPStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.MarkBackupUsed(ctx, tx, userID, "h-x")
	})
	require.ErrorIs(t, err, authapi.ErrTOTPNotEnrolled)
}

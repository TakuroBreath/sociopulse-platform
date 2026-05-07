//go:build integration

// Integration tests for auth/store.UserStore against a real Postgres 16
// instance booted via testcontainers-go. The tests apply the project's
// full migration set (000001 + 000002 + 000003), then exercise the
// store's CRUD via *postgres.Pool.WithTenant so the RLS policy is in
// effect for every read/write.
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

// insertUser is a small helper that opens a per-tenant transaction and
// calls store.Insert inside it — modelling the real service-layer call.
func insertUser(t *testing.T, ctx context.Context, pool *postgres.Pool, s *store.UserStore, in authapi.User, hash string) authapi.User {
	t.Helper()
	var out authapi.User
	require.NoError(t, pool.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		var err error
		out, err = s.Insert(ctx, tx, in, hash)
		return err
	}))
	return out
}

func TestUserStore_Insert_HappyPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-1")
	s := store.NewUserStore(pool)

	saved := insertUser(t, ctx, pool, s, authapi.User{
		TenantID:      tenantID,
		Login:         "alice",
		FullName:      "Алиса Тест",
		Email:         "alice@example.com",
		Roles:         []authapi.Role{authapi.RoleOperator},
		MustChangePwd: true,
	}, "argon2id$placeholder")

	require.NotEqual(t, uuid.Nil, saved.ID)
	require.Equal(t, tenantID, saved.TenantID)
	require.Equal(t, "alice", saved.Login)
	require.Equal(t, []authapi.Role{authapi.RoleOperator}, saved.Roles)
	require.True(t, saved.MustChangePwd)
	require.False(t, saved.TOTPEnabled)
	require.False(t, saved.CreatedAt.IsZero())
	require.Nil(t, saved.ArchivedAt)
}

func TestUserStore_Insert_UniqueLoginPerTenantViolation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-DUP")
	s := store.NewUserStore(pool)

	insertUser(t, ctx, pool, s, authapi.User{
		TenantID: tenantID,
		Login:    "dup",
		FullName: "First",
		Roles:    []authapi.Role{authapi.RoleOperator},
	}, "h1")

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.Insert(ctx, tx, authapi.User{
			TenantID: tenantID,
			Login:    "dup",
			FullName: "Second",
			Roles:    []authapi.Role{authapi.RoleSupervisor},
		}, "h2")
		return err
	})
	require.ErrorIs(t, err, authapi.ErrLoginTaken)
}

func TestUserStore_GetByID_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-GET")
	s := store.NewUserStore(pool)

	saved := insertUser(t, ctx, pool, s, authapi.User{
		TenantID: tenantID,
		Login:    "carol",
		FullName: "Carol Test",
		Email:    "carol@x.test",
		Roles:    []authapi.Role{authapi.RoleSupervisor, authapi.RoleAdmin},
	}, "h")

	var got authapi.User
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByID(ctx, tx, saved.ID)
		return err
	}))
	require.Equal(t, saved.ID, got.ID)
	require.Equal(t, "carol", got.Login)
	require.ElementsMatch(t, []authapi.Role{authapi.RoleSupervisor, authapi.RoleAdmin}, got.Roles)
}

func TestUserStore_GetByID_ReturnsErrUserNotFoundWhenMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-MISS")
	s := store.NewUserStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.GetByID(ctx, tx, uuid.New())
		return err
	})
	require.ErrorIs(t, err, authapi.ErrUserNotFound)
}

func TestUserStore_GetByLogin_CaseInsensitive(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-CI")
	s := store.NewUserStore(pool)

	insertUser(t, ctx, pool, s, authapi.User{
		TenantID: tenantID,
		Login:    "Mixed.Case",
		FullName: "Mixed",
		Roles:    []authapi.Role{authapi.RoleOperator},
	}, "h")

	var got authapi.User
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByLogin(ctx, tx, tenantID, "MIXED.case")
		return err
	}))
	require.Equal(t, "Mixed.Case", got.Login)
}

func TestUserStore_List_FiltersArchivedByDefault(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-LIST")
	s := store.NewUserStore(pool)

	for _, login := range []string{"a", "b", "c"} {
		insertUser(t, ctx, pool, s, authapi.User{
			TenantID: tenantID,
			Login:    login,
			FullName: login,
			Roles:    []authapi.Role{authapi.RoleOperator},
		}, "h")
	}

	// Archive "b".
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		got, err := s.GetByLogin(ctx, tx, tenantID, "b")
		if err != nil {
			return err
		}
		return s.Archive(ctx, tx, got.ID)
	}))

	// Default list excludes archived.
	var (
		rows  []authapi.User
		total int64
	)
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		rows, total, err = s.List(ctx, tx, authapi.ListUsersInput{
			TenantID: tenantID,
			Limit:    50,
		})
		return err
	}))
	require.Len(t, rows, 2)
	require.EqualValues(t, 2, total)

	// IncludeArchived returns the archived row too.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		rows, total, err = s.List(ctx, tx, authapi.ListUsersInput{
			TenantID:        tenantID,
			IncludeArchived: true,
			Limit:           50,
		})
		return err
	}))
	require.Len(t, rows, 3)
	require.EqualValues(t, 3, total)
}

func TestUserStore_ArchiveRestoreCycle(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-AR")
	s := store.NewUserStore(pool)

	saved := insertUser(t, ctx, pool, s, authapi.User{
		TenantID: tenantID,
		Login:    "ar",
		FullName: "AR",
		Roles:    []authapi.Role{authapi.RoleOperator},
	}, "h")

	// Archive sets archived_at.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Archive(ctx, tx, saved.ID)
	}))

	var got authapi.User
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByID(ctx, tx, saved.ID)
		return err
	}))
	require.NotNil(t, got.ArchivedAt)

	// Archive is idempotent — calling again is a no-op (still no error,
	// archived_at unchanged on the DB side from the predicate filter).
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Archive(ctx, tx, saved.ID)
	}))

	// Restore clears archived_at.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Restore(ctx, tx, saved.ID)
	}))

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByID(ctx, tx, saved.ID)
		return err
	}))
	require.Nil(t, got.ArchivedAt)

	// Restore on an active user returns ErrUserNotArchived.
	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Restore(ctx, tx, saved.ID)
	})
	require.ErrorIs(t, err, authapi.ErrUserNotArchived)
}

func TestUserStore_UpdateRoles_AndPassword(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-MUT")
	s := store.NewUserStore(pool)

	saved := insertUser(t, ctx, pool, s, authapi.User{
		TenantID:      tenantID,
		Login:         "mut",
		FullName:      "Mut",
		Roles:         []authapi.Role{authapi.RoleOperator},
		MustChangePwd: true,
	}, "old-hash")

	var refreshed authapi.User
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		refreshed, err = s.UpdateRoles(ctx, tx, saved.ID, []authapi.Role{authapi.RoleSupervisor, authapi.RoleAdmin})
		return err
	}))
	require.ElementsMatch(t,
		[]authapi.Role{authapi.RoleSupervisor, authapi.RoleAdmin},
		refreshed.Roles,
	)

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.UpdatePassword(ctx, tx, saved.ID, "new-hash", false)
	}))

	var hash string
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		hash, err = s.GetPasswordHash(ctx, tx, saved.ID)
		return err
	}))
	require.Equal(t, "new-hash", hash)
}

func TestUserStore_SetTOTPEnabled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-TOTP")
	s := store.NewUserStore(pool)

	saved := insertUser(t, ctx, pool, s, authapi.User{
		TenantID: tenantID,
		Login:    "t",
		FullName: "T",
		Roles:    []authapi.Role{authapi.RoleOperator},
	}, "h")

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.SetTOTPEnabled(ctx, tx, saved.ID, true)
	}))

	var got authapi.User
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByID(ctx, tx, saved.ID)
		return err
	}))
	require.True(t, got.TOTPEnabled)
}

func TestUserStore_Archive_ReturnsErrUserNotFoundForMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newAuthTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-USERS-ARMISS")
	s := store.NewUserStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Archive(ctx, tx, uuid.New())
	})
	require.ErrorIs(t, err, authapi.ErrUserNotFound)
}

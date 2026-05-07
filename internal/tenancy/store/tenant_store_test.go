//go:build integration

// Integration tests for tenancy/store.PostgresStore against a real Postgres 16
// instance booted via testcontainers-go. The tests apply the project's full
// migration set (000001_init + 000002_outbox), then exercise the store's
// CRUD via *postgres.Pool.BypassRLS (tenancy_admin role).
//
// Run: go test -tags=integration -count=1 -timeout 5m ./internal/tenancy/store/...
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// insertTenant is a small helper that opens a BypassRLS tx and calls
// store.Insert inside it — modelling the real service-layer call site.
func insertTenant(t *testing.T, ctx context.Context, pool *postgres.Pool, s *store.PostgresStore, in api.Tenant) api.Tenant {
	t.Helper()
	var out api.Tenant
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		var err error
		out, err = s.Insert(ctx, tx, in)
		return err
	}))
	return out
}

func TestPostgresStore_Insert_HappyPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	tn := insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode:         "CC-MOSKVA-01",
		Name:            "ВЦИОМ-Москва",
		Status:          api.TenantStatusActive,
		KMSKEKID:        "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})
	require.NotEqual(t, uuid.Nil, tn.ID)
	require.False(t, tn.CreatedAt.IsZero())
}

func TestPostgresStore_Insert_RejectsDuplicateOrgCode(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-DUP", Name: "First",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})

	err := pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		_, err := s.Insert(ctx, tx, api.Tenant{
			OrgCode: "CC-DUP", Name: "Second",
			Status: api.TenantStatusActive, KMSKEKID: "yk-kek-2",
			PhoneHashPepper: bytesOfLen(32),
		})
		return err
	})
	require.ErrorIs(t, err, api.ErrAlreadyExists)
}

func TestPostgresStore_Get_ReturnsRow(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	tn := insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-GET", Name: "Get Test",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})

	got, err := s.Get(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, tn.OrgCode, got.OrgCode)
	require.Equal(t, tn.Name, got.Name)
	require.Equal(t, api.TenantStatusActive, got.Status)
	require.Equal(t, tn.KMSKEKID, got.KMSKEKID)
	require.Equal(t, bytesOfLen(32), got.PhoneHashPepper)
}

func TestPostgresStore_Get_ReturnsErrNotFoundWhenMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	_, err := s.Get(ctx, uuid.New())
	require.ErrorIs(t, err, api.ErrNotFound)
}

func TestPostgresStore_GetByOrgCode_HappyPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	want := insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-BYORG", Name: "By Org",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})

	got, err := s.GetByOrgCode(ctx, "CC-BYORG")
	require.NoError(t, err)
	require.Equal(t, want.ID, got.ID)
}

func TestPostgresStore_GetByOrgCode_ReturnsErrNotFoundWhenMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	_, err := s.GetByOrgCode(ctx, "CC-MISSING")
	require.ErrorIs(t, err, api.ErrNotFound)
}

func TestPostgresStore_UpdateStatus_HappyPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	tn := insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-STATUS", Name: "Status",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})

	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return s.UpdateStatus(ctx, tx, tn.ID, api.TenantStatusSuspended)
	}))

	got, err := s.Get(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, api.TenantStatusSuspended, got.Status)
}

func TestPostgresStore_UpdateStatus_ReturnsErrNotFoundWhenMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	err := pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return s.UpdateStatus(ctx, tx, uuid.New(), api.TenantStatusArchived)
	})
	require.ErrorIs(t, err, api.ErrNotFound)
}

func TestPostgresStore_List_FiltersByStatus(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	tnA := insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-LISTA", Name: "A",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})
	insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-LISTB", Name: "B",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-2",
		PhoneHashPepper: bytesOfLen(32),
	})
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return s.UpdateStatus(ctx, tx, tnA.ID, api.TenantStatusSuspended)
	}))

	suspended := api.TenantStatusSuspended
	got, err := s.List(ctx, api.ListTenantsFilter{Status: &suspended, Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, tnA.ID, got[0].ID)
}

func TestPostgresStore_List_FiltersByOrgCode(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	want := insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-FIND", Name: "Find",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})
	insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-OTHER", Name: "Other",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-2",
		PhoneHashPepper: bytesOfLen(32),
	})

	got, err := s.List(ctx, api.ListTenantsFilter{OrgCode: "CC-FIND", Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, want.ID, got[0].ID)
}

func TestPostgresStore_List_RespectsLimitAndOffset(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	for i := 0; i < 5; i++ {
		insertTenant(t, ctx, pool, s, api.Tenant{
			OrgCode: "CC-PAGE-" + string(rune('A'+i)), Name: "Page",
			Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
			PhoneHashPepper: bytesOfLen(32),
		})
	}

	got, err := s.List(ctx, api.ListTenantsFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, got, 2)

	page2, err := s.List(ctx, api.ListTenantsFilter{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, page2, 2)

	require.NotEqual(t, got[0].ID, page2[0].ID)
}

func TestPostgresStore_GetPhoneHashPepper_ReturnsBytes(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	tn := insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-PEPPER", Name: "Pepper",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})

	pepper, err := s.GetPhoneHashPepper(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, bytesOfLen(32), pepper)
}

func TestPostgresStore_GetPhoneHashPepper_ReturnsErrNotFoundWhenMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	_, err := s.GetPhoneHashPepper(ctx, uuid.New())
	require.ErrorIs(t, err, api.ErrNotFound)
}

func TestPostgresStore_Settings_UpsertGetDelete(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	tn := insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-SETTINGS", Name: "Settings",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})

	v, err := api.SettingValueFromAny("4h")
	require.NoError(t, err)
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return s.UpsertSetting(ctx, tx, tn.ID, "dialer.retry_no_answer_delay", v)
	}))

	got, err := s.GetSetting(ctx, tn.ID, "dialer.retry_no_answer_delay")
	require.NoError(t, err)
	d, err := got.AsDuration()
	require.NoError(t, err)
	require.Equal(t, 4*time.Hour, d)

	all, err := s.GetAllSettings(ctx, tn.ID)
	require.NoError(t, err)
	require.Len(t, all, 1)

	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return s.DeleteSetting(ctx, tx, tn.ID, "dialer.retry_no_answer_delay")
	}))

	_, err = s.GetSetting(ctx, tn.ID, "dialer.retry_no_answer_delay")
	require.ErrorIs(t, err, api.ErrNotFound)
}

func TestPostgresStore_DeleteSetting_ReturnsErrNotFoundWhenMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newTenancyTestPool(t)
	s := store.NewPostgresStore(pool)

	tn := insertTenant(t, ctx, pool, s, api.Tenant{
		OrgCode: "CC-NOSETTING", Name: "No",
		Status: api.TenantStatusActive, KMSKEKID: "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})

	err := pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return s.DeleteSetting(ctx, tx, tn.ID, "missing.key")
	})
	require.ErrorIs(t, err, api.ErrNotFound)
}

// bytesOfLen creates a deterministic byte slice — handy for asserting
// round-trips through the bytea column.
func bytesOfLen(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

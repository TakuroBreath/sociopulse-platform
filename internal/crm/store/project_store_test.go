//go:build integration

// Integration tests for crm/store.ProjectStore against a real Postgres 16
// instance booted via testcontainers-go. The tests apply the project's
// full migration set (through 000005), then exercise the store's CRUD
// via *postgres.Pool.WithTenant so the RLS policy is in effect for
// every read/write.
//
// Run: go test -tags=integration -count=1 -timeout 5m -run Project ./internal/crm/store/...

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/crm/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// insertProject is a small helper that opens a per-tenant transaction
// and calls store.Insert inside it — modelling the real service-layer
// call pattern.
func insertProject(t *testing.T, ctx context.Context, pool *postgres.Pool, s *store.ProjectStore, in crmapi.Project) crmapi.Project {
	t.Helper()
	var out crmapi.Project
	require.NoError(t, pool.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		var err error
		out, err = s.Insert(ctx, tx, in)
		return err
	}))
	return out
}

func TestProjectStore_Insert_HappyPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-1")
	s := store.NewProjectStore(pool)

	saved := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID:    tenantID,
		Code:        "P-001",
		Name:        "Pilot Survey",
		Customer:    "ВЦИОМ",
		Status:      crmapi.StatusActive,
		TargetCount: 1000,
	})

	require.NotEqual(t, uuid.Nil, saved.ID)
	require.Equal(t, tenantID, saved.TenantID)
	require.Equal(t, "P-001", saved.Code)
	require.Equal(t, "Pilot Survey", saved.Name)
	require.Equal(t, crmapi.StatusActive, saved.Status)
	require.Equal(t, 1000, saved.TargetCount)
	require.False(t, saved.IsAdvertising)
	require.Nil(t, saved.ArchivedAt)
	require.False(t, saved.CreatedAt.IsZero())
	require.False(t, saved.UpdatedAt.IsZero())
}

func TestProjectStore_Insert_UniqueCodePerTenantViolation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-DUP")
	s := store.NewProjectStore(pool)

	insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID,
		Code:     "DUP-1",
		Name:     "First",
		Status:   crmapi.StatusActive,
	})

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.Insert(ctx, tx, crmapi.Project{
			TenantID: tenantID,
			Code:     "DUP-1",
			Name:     "Second",
			Status:   crmapi.StatusActive,
		})
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrProjectCodeTaken)
}

func TestProjectStore_GetByID_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-GET")
	s := store.NewProjectStore(pool)

	saved := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID:    tenantID,
		Code:        "G-1",
		Name:        "Get RoundTrip",
		Customer:    "Customer A",
		Status:      crmapi.StatusActive,
		TargetCount: 250,
	})

	var got crmapi.Project
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByID(ctx, tx, saved.ID)
		return err
	}))
	require.Equal(t, saved.ID, got.ID)
	require.Equal(t, "G-1", got.Code)
	require.Equal(t, "Get RoundTrip", got.Name)
	require.Equal(t, 250, got.TargetCount)
}

func TestProjectStore_GetByID_ReturnsErrProjectNotFoundWhenMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-MISS")
	s := store.NewProjectStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.GetByID(ctx, tx, uuid.New())
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}

func TestProjectStore_GetByCode_CaseInsensitive(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-CI")
	s := store.NewProjectStore(pool)

	insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID,
		Code:     "Mixed-Case",
		Name:     "Case",
		Status:   crmapi.StatusActive,
	})

	var got crmapi.Project
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByCode(ctx, tx, tenantID, "MIXED-case")
		return err
	}))
	require.Equal(t, "Mixed-Case", got.Code)
}

func TestProjectStore_GetByCode_ReturnsErrProjectNotFoundWhenMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-CMISS")
	s := store.NewProjectStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.GetByCode(ctx, tx, tenantID, "no-such-code")
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}

func TestProjectStore_List_FiltersArchivedByDefault(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-LIST")
	s := store.NewProjectStore(pool)

	for _, code := range []string{"L-A", "L-B", "L-C"} {
		insertProject(t, ctx, pool, s, crmapi.Project{
			TenantID: tenantID,
			Code:     code,
			Name:     code,
			Status:   crmapi.StatusActive,
		})
	}

	// Archive "L-B" by hand. The service-level Archive method lands in
	// Task 2; this test exercises only the List filter — the ALTER is
	// done through the same per-tenant tx so RLS stays enforced.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE projects SET archived_at = now() WHERE tenant_id = $1 AND code = $2`,
			tenantID, "L-B")
		return err
	}))

	// Default list excludes archived rows.
	var (
		rows  []crmapi.Project
		total int64
	)
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		rows, total, err = s.List(ctx, tx, crmapi.ListProjectsFilter{
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
		rows, total, err = s.List(ctx, tx, crmapi.ListProjectsFilter{
			TenantID:        tenantID,
			IncludeArchived: true,
			Limit:           50,
		})
		return err
	}))
	require.Len(t, rows, 3)
	require.EqualValues(t, 3, total)
}

func TestProjectStore_List_StatusFilter(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-LSTAT")
	s := store.NewProjectStore(pool)

	insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "S-A", Name: "Active",
		Status: crmapi.StatusActive,
	})
	insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "S-P", Name: "Paused",
		Status: crmapi.StatusPaused,
	})

	paused := crmapi.StatusPaused
	var rows []crmapi.Project
	var total int64
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		rows, total, err = s.List(ctx, tx, crmapi.ListProjectsFilter{
			TenantID: tenantID,
			Status:   &paused,
			Limit:    50,
		})
		return err
	}))
	require.Len(t, rows, 1)
	require.EqualValues(t, 1, total)
	require.Equal(t, "S-P", rows[0].Code)
}

func TestProjectStore_List_Pagination(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-LPAG")
	s := store.NewProjectStore(pool)

	for _, code := range []string{"PG-1", "PG-2", "PG-3", "PG-4", "PG-5"} {
		insertProject(t, ctx, pool, s, crmapi.Project{
			TenantID: tenantID, Code: code, Name: code,
			Status: crmapi.StatusActive,
		})
	}

	var rows []crmapi.Project
	var total int64
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		rows, total, err = s.List(ctx, tx, crmapi.ListProjectsFilter{
			TenantID: tenantID,
			Limit:    2,
			Offset:   2,
		})
		return err
	}))
	require.Len(t, rows, 2, "limit=2 must clamp to 2 rows")
	require.EqualValues(t, 5, total, "total reflects unfiltered count")
}

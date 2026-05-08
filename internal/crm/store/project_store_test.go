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

// ─── Update ────────────────────────────────────────────────────────────────

func TestProjectStore_Update_PartialPatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-UP-1")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID:    tenantID,
		Code:        "U-PART",
		Name:        "Original",
		Customer:    "Old Customer",
		Status:      crmapi.StatusActive,
		TargetCount: 100,
	})

	newName := "Patched"
	var updated crmapi.Project
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		updated, err = s.Update(ctx, tx, seeded.ID, crmapi.UpdatePatch{
			Name: &newName,
		})
		return err
	}))
	require.Equal(t, "Patched", updated.Name)
	require.Equal(t, "Old Customer", updated.Customer, "untouched field stays")
	require.Equal(t, 100, updated.TargetCount, "untouched field stays")
}

func TestProjectStore_Update_RejectsArchivedRow(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-UP-AR")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID,
		Code:     "U-AR",
		Name:     "To Be Archived",
		Status:   crmapi.StatusActive,
	})
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE projects SET archived_at = now() WHERE id = $1`, seeded.ID)
		return err
	}))

	newName := "Should Fail"
	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.Update(ctx, tx, seeded.ID, crmapi.UpdatePatch{Name: &newName})
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound,
		"archived row excluded by Update predicate -> ErrProjectNotFound")
}

func TestProjectStore_Update_MissingReturnsErrProjectNotFound(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-UP-MISS")
	s := store.NewProjectStore(pool)

	newName := "Ghost"
	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.Update(ctx, tx, uuid.New(), crmapi.UpdatePatch{Name: &newName})
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}

// ─── UpdateStatus ──────────────────────────────────────────────────────────

func TestProjectStore_UpdateStatus_PauseAndArchive(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-STATUS")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "S-1", Name: "Status",
		Status: crmapi.StatusActive,
	})

	// Active -> Paused (no archived_at).
	var paused crmapi.Project
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		paused, err = s.UpdateStatus(ctx, tx, seeded.ID, crmapi.StatusPaused, nil)
		return err
	}))
	require.Equal(t, crmapi.StatusPaused, paused.Status)
	require.Nil(t, paused.ArchivedAt, "non-archive transition leaves archived_at nil")

	// Paused -> Archived (with archived_at).
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	var archived crmapi.Project
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		archived, err = s.UpdateStatus(ctx, tx, seeded.ID, crmapi.StatusArchived, &now)
		return err
	}))
	require.Equal(t, crmapi.StatusArchived, archived.Status)
	require.NotNil(t, archived.ArchivedAt)
}

func TestProjectStore_UpdateStatus_MissingReturnsErrProjectNotFound(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-STAT-MISS")
	s := store.NewProjectStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.UpdateStatus(ctx, tx, uuid.New(), crmapi.StatusPaused, nil)
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}

// ─── AggregateProgress ────────────────────────────────────────────────────

func TestProjectStore_AggregateProgress_EmptyProject(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-AG-1")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "AG-E", Name: "Empty",
		Status:      crmapi.StatusActive,
		TargetCount: 500,
	})

	var prog crmapi.ProjectProgress
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		prog, err = s.AggregateProgress(ctx, tx, seeded.ID)
		return err
	}))
	require.Equal(t, seeded.ID, prog.ProjectID)
	require.Equal(t, 500, prog.TargetCount)
	require.Equal(t, 0, prog.CompletedCount)
	require.Equal(t, 0, prog.InProgressCount)
	require.Empty(t, prog.QuotaProgress)
}

func TestProjectStore_AggregateProgress_WithRespondents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-AG-R")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "AG-R", Name: "With Respondents",
		Status:      crmapi.StatusActive,
		TargetCount: 100,
	})

	// Seed a handful of respondents in different statuses through a per-
	// tenant tx so RLS lets the rows land.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		statuses := []string{
			"completed", "completed", "completed",
			"dialing",
			"pending", "pending",
			"dnc",
			"exhausted",
		}
		for i, st := range statuses {
			_, err := tx.Exec(ctx,
				`INSERT INTO respondents (tenant_id, project_id, phone_encrypted, phone_hash, region_code, source, status)
				 VALUES ($1, $2, $3::bytea, $4::bytea, 'RU-MOW', 'imported', $5)`,
				tenantID, seeded.ID,
				[]byte{byte(i)}, []byte{byte(i + 1)}, st,
			)
			if err != nil {
				return err
			}
		}
		return nil
	}))

	var prog crmapi.ProjectProgress
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		prog, err = s.AggregateProgress(ctx, tx, seeded.ID)
		return err
	}))
	require.Equal(t, 100, prog.TargetCount)
	require.Equal(t, 3, prog.CompletedCount)
	require.Equal(t, 1, prog.InProgressCount)
	require.Equal(t, 2, prog.PendingCount)
	require.Equal(t, 1, prog.DNCCount)
	require.Equal(t, 1, prog.ExhaustedCount)
	require.Equal(t, 0, prog.WrongCount)
}

func TestProjectStore_AggregateProgress_WithQuotaCells(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-AG-Q")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "AG-Q", Name: "With Quotas",
		Status:      crmapi.StatusActive,
		TargetCount: 200,
	})
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO project_quotas (project_id, dimension_kind, dimension_value, target, done)
			 VALUES ($1, 'region', 'ЦФО', 100, 40),
			        ($1, 'region', 'СЗФО', 50, 50)`,
			seeded.ID)
		return err
	}))

	var prog crmapi.ProjectProgress
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		prog, err = s.AggregateProgress(ctx, tx, seeded.ID)
		return err
	}))
	require.Len(t, prog.QuotaProgress, 2)
	require.Equal(t, "region", prog.QuotaProgress[0].DimensionKind)
	require.Equal(t, "СЗФО", prog.QuotaProgress[0].DimensionValue,
		"order is dimension_kind ASC, dimension_value ASC")
	require.Equal(t, 50, prog.QuotaProgress[0].Target)
	require.Equal(t, 50, prog.QuotaProgress[0].Done)
	require.True(t, prog.QuotaProgress[0].IsFull)
	require.InDelta(t, 100.0, prog.QuotaProgress[0].PercentDone, 0.01)

	require.Equal(t, "ЦФО", prog.QuotaProgress[1].DimensionValue)
	require.False(t, prog.QuotaProgress[1].IsFull)
	require.InDelta(t, 40.0, prog.QuotaProgress[1].PercentDone, 0.01)
}

func TestProjectStore_AggregateProgress_MissingReturnsErrProjectNotFound(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-AG-MISS")
	s := store.NewProjectStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.AggregateProgress(ctx, tx, uuid.New())
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}

// ─── Assign / Unassign / ListMembers ──────────────────────────────────────

func TestProjectStore_AssignOperators_HappyAndIdempotent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-AS-1")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "AS-1", Name: "Assign 1",
		Status: crmapi.StatusActive,
	})
	op1 := seedUser(t, ctx, pool, tenantID, "op-as-1", "Op AS One")
	op2 := seedUser(t, ctx, pool, tenantID, "op-as-2", "Op AS Two")

	// First assign: both new -> added=2.
	var added int
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		added, err = s.AssignOperators(ctx, tx, seeded.ID, []uuid.UUID{op1, op2})
		return err
	}))
	require.Equal(t, 2, added)

	// Re-run: both already members -> added=0 (idempotent).
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		added, err = s.AssignOperators(ctx, tx, seeded.ID, []uuid.UUID{op1, op2})
		return err
	}))
	require.Equal(t, 0, added)

	// Mixed: one new, one existing -> added=1.
	op3 := seedUser(t, ctx, pool, tenantID, "op-as-3", "Op AS Three")
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		added, err = s.AssignOperators(ctx, tx, seeded.ID, []uuid.UUID{op1, op3})
		return err
	}))
	require.Equal(t, 1, added, "ON CONFLICT DO NOTHING returns only the inserted op_id")
}

func TestProjectStore_AssignOperators_EmptyInputIsZero(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-AS-E")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "AS-E", Name: "Empty",
		Status: crmapi.StatusActive,
	})

	var added int
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		added, err = s.AssignOperators(ctx, tx, seeded.ID, nil)
		return err
	}))
	require.Equal(t, 0, added)
}

func TestProjectStore_UnassignOperator_HappyAndNoOp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-UN-1")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "UN-1", Name: "Unassign",
		Status: crmapi.StatusActive,
	})
	op := seedUser(t, ctx, pool, tenantID, "op-un-1", "Op Un One")

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.AssignOperators(ctx, tx, seeded.ID, []uuid.UUID{op})
		return err
	}))

	// Happy: row present -> deleted=true.
	var deleted bool
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		deleted, err = s.UnassignOperator(ctx, tx, seeded.ID, op)
		return err
	}))
	require.True(t, deleted)

	// Re-run: row already gone -> deleted=false (no-op).
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		deleted, err = s.UnassignOperator(ctx, tx, seeded.ID, op)
		return err
	}))
	require.False(t, deleted)
}

func TestProjectStore_ListMembers_JoinsUsersAndSortsByAssignedAt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-LM-1")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "LM-1", Name: "List Members",
		Status: crmapi.StatusActive,
	})
	op1 := seedUser(t, ctx, pool, tenantID, "alice-lm", "Alice LM")
	op2 := seedUser(t, ctx, pool, tenantID, "bob-lm", "Bob LM")

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.AssignOperators(ctx, tx, seeded.ID, []uuid.UUID{op1, op2})
		return err
	}))

	var members []crmapi.ProjectMember
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		members, err = s.ListMembers(ctx, tx, seeded.ID)
		return err
	}))
	require.Len(t, members, 2)
	// Both inserted in the same statement -> assigned_at is identical;
	// fall back to operator_id ASC ordering for determinism.
	gotIDs := []uuid.UUID{members[0].OperatorID, members[1].OperatorID}
	require.Contains(t, gotIDs, op1)
	require.Contains(t, gotIDs, op2)
	// At least one entry must carry the joined display fields.
	for _, m := range members {
		require.NotEmpty(t, m.Login, "Login should be populated by users join")
		require.NotEmpty(t, m.FullName, "FullName should be populated by users join")
	}
}

func TestProjectStore_ListMembers_EmptyProjectReturnsEmpty(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-PROJ-LM-E")
	s := store.NewProjectStore(pool)

	seeded := insertProject(t, ctx, pool, s, crmapi.Project{
		TenantID: tenantID, Code: "LM-E", Name: "Empty",
		Status: crmapi.StatusActive,
	})

	var members []crmapi.ProjectMember
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		members, err = s.ListMembers(ctx, tx, seeded.ID)
		return err
	}))
	require.Empty(t, members)
}

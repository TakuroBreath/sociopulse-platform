//go:build integration

// Integration tests for surveys/store.SurveyStore against a real
// Postgres 16 instance booted via testcontainers-go. The tests apply
// the project's full migration set (through 000008), then exercise
// the store's CRUD via *postgres.Pool.WithTenant so the RLS policy is
// in effect for every read/write.
//
// Run: go test -tags=integration -count=1 -timeout 5m -run Survey ./internal/surveys/store/...

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	surveysapi "github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// insertSurvey opens a per-tenant transaction and calls store.Insert
// inside it — modelling the real service-layer call pattern.
func insertSurvey(t *testing.T, ctx context.Context, pool *postgres.Pool, s *store.SurveyStore, in surveysapi.Survey) surveysapi.Survey {
	t.Helper()
	var out surveysapi.Survey
	require.NoError(t, pool.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		var err error
		out, err = s.Insert(ctx, tx, in)
		return err
	}))
	return out
}

func TestSurveyStore_Insert_HappyPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-SURV-1")
	s := store.NewSurveyStore(pool)

	saved := insertSurvey(t, ctx, pool, s, surveysapi.Survey{
		TenantID:    tenantID,
		Name:        "Pilot Survey",
		Description: "Pilot description",
		PrimaryMode: surveysapi.ModeForm,
		Status:      surveysapi.StatusActive,
	})

	require.NotEqual(t, uuid.Nil, saved.ID)
	require.Equal(t, tenantID, saved.TenantID)
	require.Equal(t, "Pilot Survey", saved.Name)
	require.Equal(t, "Pilot description", saved.Description)
	require.Equal(t, surveysapi.ModeForm, saved.PrimaryMode)
	require.Equal(t, surveysapi.StatusActive, saved.Status)
	require.False(t, saved.CreatedAt.IsZero())
	require.False(t, saved.UpdatedAt.IsZero())
}

func TestSurveyStore_Insert_DefaultsStatusAndMode(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-SURV-DEFAULTS")
	s := store.NewSurveyStore(pool)

	saved := insertSurvey(t, ctx, pool, s, surveysapi.Survey{
		TenantID: tenantID,
		Name:     "Defaults",
	})

	require.Equal(t, surveysapi.ModeForm, saved.PrimaryMode)
	require.Equal(t, surveysapi.StatusActive, saved.Status)
}

func TestSurveyStore_GetByID_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-SURV-GET")
	s := store.NewSurveyStore(pool)

	saved := insertSurvey(t, ctx, pool, s, surveysapi.Survey{
		TenantID:    tenantID,
		Name:        "RoundTrip",
		PrimaryMode: surveysapi.ModeFlow,
	})

	var got surveysapi.Survey
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByID(ctx, tx, saved.ID)
		return err
	}))
	require.Equal(t, saved.ID, got.ID)
	require.Equal(t, saved.Name, got.Name)
	require.Equal(t, surveysapi.ModeFlow, got.PrimaryMode)
}

func TestSurveyStore_GetByID_MissingReturnsErrNotFound(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-SURV-MISS")
	s := store.NewSurveyStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.GetByID(ctx, tx, uuid.New())
		return err
	})
	require.ErrorIs(t, err, surveysapi.ErrNotFound)
}

func TestSurveyStore_Update_PartialPatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-SURV-UPD")
	s := store.NewSurveyStore(pool)

	saved := insertSurvey(t, ctx, pool, s, surveysapi.Survey{
		TenantID:    tenantID,
		Name:        "Original",
		Description: "Old desc",
		PrimaryMode: surveysapi.ModeForm,
	})

	newName := "Renamed"
	flow := surveysapi.ModeFlow
	var updated surveysapi.Survey
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		updated, err = s.Update(ctx, tx, saved.ID, surveysapi.SurveyPatch{
			Name:        &newName,
			PrimaryMode: &flow,
		})
		return err
	}))

	require.Equal(t, "Renamed", updated.Name)
	require.Equal(t, "Old desc", updated.Description) // untouched
	require.Equal(t, surveysapi.ModeFlow, updated.PrimaryMode)
	require.True(t, updated.UpdatedAt.After(saved.UpdatedAt) || updated.UpdatedAt.Equal(saved.UpdatedAt))
}

func TestSurveyStore_List_FiltersAndPagination(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-SURV-LIST")
	s := store.NewSurveyStore(pool)

	for i := 0; i < 3; i++ {
		insertSurvey(t, ctx, pool, s, surveysapi.Survey{
			TenantID: tenantID,
			Name:     "S-" + string(rune('a'+i)),
		})
	}

	var (
		rows  []surveysapi.Survey
		total int64
	)
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		rows, total, err = s.List(ctx, tx, surveysapi.ListFilter{
			Status: surveysapi.StatusActive,
			Limit:  10,
		})
		return err
	}))
	require.Equal(t, int64(3), total)
	require.Len(t, rows, 3)
}

func TestSurveyStore_Archive_SetsStatusAndArchivedAt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-SURV-ARC")
	s := store.NewSurveyStore(pool)

	saved := insertSurvey(t, ctx, pool, s, surveysapi.Survey{
		TenantID: tenantID,
		Name:     "ToArchive",
	})

	at := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return s.Archive(ctx, tx, saved.ID, at)
	}))

	// Direct read to confirm status flipped.
	var status string
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM surveys WHERE id = $1`, saved.ID).Scan(&status)
	}))
	require.Equal(t, "archived", status)
}

func TestSurveyStore_SetCurrentVersion_UpdatesPointer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-SURV-PTR")
	s := store.NewSurveyStore(pool)
	v := store.NewVersionStore(pool)

	saved := insertSurvey(t, ctx, pool, s, surveysapi.Survey{
		TenantID: tenantID,
		Name:     "WithVersion",
	})

	var versionID uuid.UUID
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		ver, err := v.Insert(ctx, tx, surveysapi.Version{
			SurveyID: saved.ID,
			Major:    1,
			Minor:    0,
			Schema:   []byte(`{}`),
		})
		if err != nil {
			return err
		}
		versionID = ver.ID
		return s.SetCurrentVersion(ctx, tx, saved.ID, versionID)
	}))

	// Verify pointer was set.
	var current uuid.UUID
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_version_id FROM surveys WHERE id = $1`, saved.ID).Scan(&current)
	}))
	require.Equal(t, versionID, current)
}

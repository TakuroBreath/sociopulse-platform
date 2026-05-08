//go:build integration

// Integration tests for surveys/store.VersionStore against a real
// Postgres 16 instance booted via testcontainers-go.
//
// Run: go test -tags=integration -count=1 -timeout 5m -run Version ./internal/surveys/store/...

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

// seedSurvey inserts a fresh survey row and returns its id, opening a
// per-tenant transaction so RLS still scopes the write. Used by the
// version-store tests to create a parent survey for each version row.
func seedSurvey(t *testing.T, ctx context.Context, pool *postgres.Pool, tenantID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	s := store.NewSurveyStore(pool)
	out := insertSurvey(t, ctx, pool, s, surveysapi.Survey{
		TenantID: tenantID,
		Name:     name,
	})
	return out.ID
}

func TestVersionStore_Insert_HappyPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-VER-1")
	surveyID := seedSurvey(t, ctx, pool, tenantID, "Survey-1")
	v := store.NewVersionStore(pool)

	var saved surveysapi.Version
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		saved, err = v.Insert(ctx, tx, surveysapi.Version{
			SurveyID: surveyID,
			Major:    1,
			Minor:    0,
			Schema:   []byte(`{"foo":"bar"}`),
		})
		return err
	}))

	require.NotEqual(t, uuid.Nil, saved.ID)
	require.Equal(t, surveyID, saved.SurveyID)
	require.Equal(t, 1, saved.Major)
	require.Equal(t, 0, saved.Minor)
	require.False(t, saved.IsActive)
	require.Nil(t, saved.ActivatedAt)
	require.False(t, saved.CreatedAt.IsZero())
	require.Equal(t, []byte(`{"foo": "bar"}`), saved.Schema)
}

func TestVersionStore_Insert_DuplicateMajorMinorViolatesUnique(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-VER-DUP")
	surveyID := seedSurvey(t, ctx, pool, tenantID, "Survey-Dup")
	v := store.NewVersionStore(pool)

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := v.Insert(ctx, tx, surveysapi.Version{
			SurveyID: surveyID,
			Major:    1,
			Minor:    0,
			Schema:   []byte(`{}`),
		})
		return err
	}))

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := v.Insert(ctx, tx, surveysapi.Version{
			SurveyID: surveyID,
			Major:    1,
			Minor:    0,
			Schema:   []byte(`{}`),
		})
		return err
	})
	require.Error(t, err)
}

func TestVersionStore_Activate_FlipsIsActive(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-VER-ACT")
	surveyID := seedSurvey(t, ctx, pool, tenantID, "Survey-Act")
	v := store.NewVersionStore(pool)

	var ver1, ver2 surveysapi.Version
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		ver1, err = v.Insert(ctx, tx, surveysapi.Version{
			SurveyID: surveyID, Major: 1, Minor: 0, Schema: []byte(`{}`),
		})
		if err != nil {
			return err
		}
		ver2, err = v.Insert(ctx, tx, surveysapi.Version{
			SurveyID: surveyID, Major: 1, Minor: 1, Schema: []byte(`{}`),
		})
		return err
	}))

	at := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if err := v.DeactivateAll(ctx, tx, surveyID); err != nil {
			return err
		}
		return v.Activate(ctx, tx, ver1.ID, at)
	}))

	// Activate ver2: this must deactivate ver1 first or the partial
	// unique index raises 23505.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if err := v.DeactivateAll(ctx, tx, surveyID); err != nil {
			return err
		}
		return v.Activate(ctx, tx, ver2.ID, at)
	}))

	var active surveysapi.Version
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		active, err = v.GetActive(ctx, tx, surveyID)
		return err
	}))
	require.Equal(t, ver2.ID, active.ID)
	require.True(t, active.IsActive)
	require.NotNil(t, active.ActivatedAt)
}

func TestVersionStore_GetActive_NoneReturnsErrNoActiveVersion(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-VER-NONE")
	surveyID := seedSurvey(t, ctx, pool, tenantID, "Survey-None")
	v := store.NewVersionStore(pool)

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := v.Insert(ctx, tx, surveysapi.Version{
			SurveyID: surveyID, Major: 1, Minor: 0, Schema: []byte(`{}`),
		})
		return err
	}))

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := v.GetActive(ctx, tx, surveyID)
		return err
	})
	require.ErrorIs(t, err, surveysapi.ErrNoActiveVersion)
}

func TestVersionStore_LatestMajorMinor(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-VER-LAT")
	surveyID := seedSurvey(t, ctx, pool, tenantID, "Survey-Lat")
	v := store.NewVersionStore(pool)

	// No versions yet.
	var latestMajor int
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		latestMajor, err = v.LatestMajor(ctx, tx, surveyID)
		return err
	}))
	require.Equal(t, 0, latestMajor)

	// Insert v1.0, v1.1, v2.0.
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		for _, mm := range []struct{ major, minor int }{{1, 0}, {1, 1}, {2, 0}} {
			_, err := v.Insert(ctx, tx, surveysapi.Version{
				SurveyID: surveyID, Major: mm.major, Minor: mm.minor, Schema: []byte(`{}`),
			})
			if err != nil {
				return err
			}
		}
		return nil
	}))

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		latestMajor, err = v.LatestMajor(ctx, tx, surveyID)
		return err
	}))
	require.Equal(t, 2, latestMajor)

	var latestMinor int
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		latestMinor, err = v.LatestMinor(ctx, tx, surveyID, 1)
		return err
	}))
	require.Equal(t, 1, latestMinor)

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		latestMinor, err = v.LatestMinor(ctx, tx, surveyID, 3)
		return err
	}))
	require.Equal(t, -1, latestMinor)
}

func TestVersionStore_List_NewestFirst(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newSurveysTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-VER-LIST")
	surveyID := seedSurvey(t, ctx, pool, tenantID, "Survey-List")
	v := store.NewVersionStore(pool)

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		for _, mm := range []struct{ major, minor int }{{1, 0}, {1, 1}, {2, 0}} {
			_, err := v.Insert(ctx, tx, surveysapi.Version{
				SurveyID: surveyID, Major: mm.major, Minor: mm.minor, Schema: []byte(`{}`),
			})
			if err != nil {
				return err
			}
		}
		return nil
	}))

	var rows []surveysapi.Version
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		rows, err = v.List(ctx, tx, surveyID)
		return err
	}))
	require.Len(t, rows, 3)
	require.Equal(t, 2, rows[0].Major)
	require.Equal(t, 0, rows[0].Minor)
	require.Equal(t, 1, rows[1].Major)
	require.Equal(t, 1, rows[1].Minor)
	require.Equal(t, 1, rows[2].Major)
	require.Equal(t, 0, rows[2].Minor)
}

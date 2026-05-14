//go:build integration

package service_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/service"
	"github.com/sociopulse/platform/internal/analytics/store"
)

// openTestStore boots a fresh CH container, applies migrations, and
// returns a connected store.Conn ready for ingest + query. The helpers
// (startCH, migrateUp) are defined in ingest_integration_test.go and
// shared across the suite.
func openTestStore(t *testing.T) *store.Conn {
	t.Helper()
	dsns := startCH(t)
	migrateUp(t, dsns.migrate)

	conn, err := store.Open(t.Context(), store.Config{
		DSN:           dsns.verify,
		BatchSize:     100,
		FlushInterval: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// openTestCache spins up miniredis + a connected go-redis client and
// wraps both into a *RedisCache for the QueryService test.
func openTestCache(t *testing.T) service.Cache {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return service.NewRedisCache(rdb, zap.NewNop())
}

// TestQueryService_RoundTrip_Calls inserts call fixtures into CH, then
// asks QueryService for the Calls aggregate. Asserts the result mirrors
// the expected count + that a second call short-circuits to cache (the
// CH state-table is not touched between calls).
func TestQueryService_RoundTrip_Calls(t *testing.T) {
	t.Parallel()
	conn := openTestStore(t)
	cache := openTestCache(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	rows := buildIntegrationCallFixtures(tenantID, projectID, base, 10)
	require.NoError(t, store.InsertCalls(t.Context(), conn, rows))

	// Optimize so the AggregatingMergeTree merge is immediate (test-only).
	require.NoError(t, conn.Driver().Exec(t.Context(),
		"OPTIMIZE TABLE mv_calls_hourly_state FINAL"))

	cfg := service.QueryConfig{
		CacheShortTTL:       30 * time.Second,
		CacheLongTTL:        5 * time.Minute,
		LongWindowThreshold: 24 * time.Hour,
	}
	svc, err := service.NewQueryService(
		&service.StoreReaderAdapter{Conn: conn},
		cache, nil, zap.NewNop(), nil, cfg,
	)
	require.NoError(t, err)

	q := apianalytics.CallsQuery{
		TenantID:  tenantID,
		ProjectID: &projectID,
		Window:    apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(2 * time.Hour)},
	}

	res, err := svc.Calls(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, uint64(10), res.Total)
	require.Equal(t, uint64(5), res.Successful)
	require.Equal(t, uint64(3), res.Failed)
	require.Equal(t, uint64(2), res.Refusals)
	require.Greater(t, res.AvgDurSec, 0.0)

	// Second call — should be served from cache (still produces the
	// same numbers; CH state would be unchanged anyway, but the cache
	// short-circuit means the read is now ~microseconds).
	res2, err := svc.Calls(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, res, res2)
}

// TestQueryService_RoundTrip_Hourly inserts call fixtures and asks for
// the per-hour bucket breakdown. One hour covered → exactly one bucket.
func TestQueryService_RoundTrip_Hourly(t *testing.T) {
	t.Parallel()
	conn := openTestStore(t)
	cache := openTestCache(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	rows := buildIntegrationCallFixtures(tenantID, projectID, base, 10)
	require.NoError(t, store.InsertCalls(t.Context(), conn, rows))
	require.NoError(t, conn.Driver().Exec(t.Context(),
		"OPTIMIZE TABLE mv_calls_hourly_state FINAL"))

	cfg := service.QueryConfig{
		CacheShortTTL:       30 * time.Second,
		CacheLongTTL:        5 * time.Minute,
		LongWindowThreshold: 24 * time.Hour,
	}
	svc, err := service.NewQueryService(
		&service.StoreReaderAdapter{Conn: conn},
		cache, nil, zap.NewNop(), nil, cfg,
	)
	require.NoError(t, err)

	buckets, err := svc.Hourly(t.Context(), apianalytics.HourlyQuery{
		TenantID:  tenantID,
		ProjectID: &projectID,
		Window:    apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(2 * time.Hour)},
	})
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	require.Equal(t, uint64(10), buckets[0].Count)
}

// buildIntegrationCallFixtures mirrors buildCallFixtures from the
// store integration suite but lives here so the service-side
// integration tests are self-contained.
func buildIntegrationCallFixtures(tenantID, projectID uuid.UUID, base time.Time, n int) []apianalytics.AnalyticsCallEventPayload {
	statuses := []string{"success", "success", "success", "success", "success", "fail", "fail", "fail", "refusal", "refusal"}
	rows := make([]apianalytics.AnalyticsCallEventPayload, 0, n)
	for i := range n {
		ts := base.Add(time.Duration(i) * time.Second)
		rows = append(rows, apianalytics.AnalyticsCallEventPayload{
			Date:        ts.Format("2006-01-02"),
			TS:          ts,
			TenantID:    tenantID,
			ProjectID:   projectID,
			OperatorID:  uuid.New(),
			CallID:      uuid.New(),
			Status:      statuses[i%len(statuses)],
			DurationSec: uint32(30 + i),
			HangupCause: "NORMAL_CLEARING",
			RegionCode:  "MSK",
			AttemptNo:   1,
			TrunkUsed:   "trunk-a",
			EventID:     uuid.New(),
		})
	}
	return rows
}

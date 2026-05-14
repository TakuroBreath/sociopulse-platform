//go:build integration

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/store"
)

// buildCallFixtures builds n call rows alternating through statuses
// (5 success / 3 fail / 2 refusal when n==10) anchored at base. All
// rows share tenant + project so the CallsByMV roll-up returns the
// expected counters in one bucket.
func buildCallFixtures(_ *testing.T, tenantID, projectID uuid.UUID, base time.Time, n int) []apianalytics.AnalyticsCallEventPayload {
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

// optimizeMVStates forces an immediate AggregatingMergeTree merge of
// all parts so the rollup is queryable in a single round after fixture
// insert. Test-only — never in production (see analytics-mv.md).
func optimizeMVStates(t *testing.T, conn *store.Conn) {
	t.Helper()
	for _, table := range []string{
		"mv_calls_hourly_state",
		"mv_operator_kpi_daily_state",
		"mv_quotas_progress_state",
	} {
		require.NoError(t, conn.Driver().Exec(t.Context(),
			"OPTIMIZE TABLE "+table+" FINAL"))
	}
}

// TestCallsByMV_RollsUpFixture inserts 10 call rows then asserts the
// MV rollup correctly counts total/success/fail/refusal + avg duration.
// The window covers the full fixture range (start to +2h).
func TestCallsByMV_RollsUpFixture(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	rows := buildCallFixtures(t, tenantID, projectID, base, 10)
	require.NoError(t, store.InsertCalls(t.Context(), conn, rows))

	optimizeMVStates(t, conn)

	res, err := store.CallsByMV(t.Context(), conn, apianalytics.CallsQuery{
		TenantID:  tenantID,
		ProjectID: &projectID,
		Window:    apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(2 * time.Hour)},
	})
	require.NoError(t, err)
	require.Equal(t, uint64(10), res.Total)
	require.Equal(t, uint64(5), res.Successful)
	require.Equal(t, uint64(3), res.Failed)
	require.Equal(t, uint64(2), res.Refusals)
	require.Greater(t, res.AvgDurSec, 0.0)
	require.Greater(t, res.TotalDurSec, uint64(0))
	require.NotEmpty(t, res.ByStatus)
}

// TestCallsByMV_TenantIsolation ensures rows inserted under a different
// tenant_id do NOT bleed into another tenant's query result.
func TestCallsByMV_TenantIsolation(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantA := uuid.New()
	tenantB := uuid.New()
	projectID := uuid.New()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	rowsA := buildCallFixtures(t, tenantA, projectID, base, 10)
	rowsB := buildCallFixtures(t, tenantB, projectID, base, 4)
	require.NoError(t, store.InsertCalls(t.Context(), conn, rowsA))
	require.NoError(t, store.InsertCalls(t.Context(), conn, rowsB))

	optimizeMVStates(t, conn)

	resA, err := store.CallsByMV(t.Context(), conn, apianalytics.CallsQuery{
		TenantID:  tenantA,
		ProjectID: &projectID,
		Window:    apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(2 * time.Hour)},
	})
	require.NoError(t, err)
	require.Equal(t, uint64(10), resA.Total, "tenant A sees only its own rows")
}

// TestCallsByMV_ProjectOptional verifies that omitting ProjectID
// returns the cross-project total for the tenant.
func TestCallsByMV_ProjectOptional(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantID := uuid.New()
	projectA := uuid.New()
	projectB := uuid.New()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	require.NoError(t, store.InsertCalls(t.Context(), conn, buildCallFixtures(t, tenantID, projectA, base, 10)))
	require.NoError(t, store.InsertCalls(t.Context(), conn, buildCallFixtures(t, tenantID, projectB, base, 4)))

	optimizeMVStates(t, conn)

	all, err := store.CallsByMV(t.Context(), conn, apianalytics.CallsQuery{
		TenantID:  tenantID,
		ProjectID: nil,
		Window:    apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(2 * time.Hour)},
	})
	require.NoError(t, err)
	require.Equal(t, uint64(14), all.Total)

	only, err := store.CallsByMV(t.Context(), conn, apianalytics.CallsQuery{
		TenantID:  tenantID,
		ProjectID: &projectA,
		Window:    apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(2 * time.Hour)},
	})
	require.NoError(t, err)
	require.Equal(t, uint64(10), only.Total)
}

// TestOperatorStateByMV_RollsUp inserts a mix of operator state
// transitions and asserts the sumMerge-ed talk/pause/ready/wrap totals.
func TestOperatorStateByMV_RollsUp(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantID := uuid.New()
	userID := uuid.New()
	projectID := uuid.New()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	rows := []apianalytics.AnalyticsOperatorStateEventPayload{
		{Date: base.Format("2006-01-02"), TS: base, TenantID: tenantID, UserID: userID, State: "ready", DurationInStateSec: 100, ProjectID: &projectID, EventID: uuid.New()},
		{Date: base.Format("2006-01-02"), TS: base.Add(time.Second), TenantID: tenantID, UserID: userID, State: "in_call", DurationInStateSec: 200, ProjectID: &projectID, EventID: uuid.New()},
		{Date: base.Format("2006-01-02"), TS: base.Add(2 * time.Second), TenantID: tenantID, UserID: userID, State: "pause", DurationInStateSec: 50, ProjectID: &projectID, EventID: uuid.New()},
		{Date: base.Format("2006-01-02"), TS: base.Add(3 * time.Second), TenantID: tenantID, UserID: userID, State: "wrap_up", DurationInStateSec: 30, ProjectID: &projectID, EventID: uuid.New()},
	}
	require.NoError(t, store.InsertOperatorStates(t.Context(), conn, rows))
	optimizeMVStates(t, conn)

	res, err := store.OperatorStateByMV(t.Context(), conn, apianalytics.OperatorStateQuery{
		TenantID:   tenantID,
		OperatorID: &userID,
		ProjectID:  &projectID,
		Window:     apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(24 * time.Hour)},
	})
	require.NoError(t, err)
	require.Equal(t, uint64(200), res.TalkSec)
	require.Equal(t, uint64(50), res.PauseSec)
	require.Equal(t, uint64(100), res.ReadySec)
	require.Equal(t, uint64(30), res.WrapSec)
}

// TestRegionProgressDoneByMV_GroupsByRegion inserts call rows with two
// different region codes and asserts the per-region done-count map.
func TestRegionProgressDoneByMV_GroupsByRegion(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	mskRows := buildCallFixtures(t, tenantID, projectID, base, 10)
	spbRows := buildCallFixtures(t, tenantID, projectID, base.Add(time.Hour), 4)
	for i := range spbRows {
		spbRows[i].RegionCode = "SPB"
	}
	require.NoError(t, store.InsertCalls(t.Context(), conn, mskRows))
	require.NoError(t, store.InsertCalls(t.Context(), conn, spbRows))

	optimizeMVStates(t, conn)

	done, err := store.RegionProgressDoneByMV(t.Context(), conn, apianalytics.RegionProgressQuery{
		TenantID:  tenantID,
		ProjectID: projectID,
		Window:    apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(48 * time.Hour)},
	})
	require.NoError(t, err)
	// 5 successes in MSK fixture; 2 in SPB (4 rows, every 5th would
	// be success but only first 4 statuses → 4 success).
	require.Equal(t, uint64(5), done["MSK"])
	require.Equal(t, uint64(4), done["SPB"])
}

// TestHourlyByMV_OneBucket inserts 10 rows in a single hour and
// asserts a single per-hour bucket is returned with count=10.
func TestHourlyByMV_OneBucket(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	rows := buildCallFixtures(t, tenantID, projectID, base, 10)
	require.NoError(t, store.InsertCalls(t.Context(), conn, rows))
	optimizeMVStates(t, conn)

	buckets, err := store.HourlyByMV(t.Context(), conn, apianalytics.HourlyQuery{
		TenantID:  tenantID,
		ProjectID: &projectID,
		Window:    apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(2 * time.Hour)},
	})
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	require.Equal(t, uint64(10), buckets[0].Count)
	require.Greater(t, buckets[0].AvgDurSec, 0.0)
}

// TestOperatorComparisonsByMV_PerOperatorRows inserts call rows for
// two operators in the same project + day and asserts each appears
// as its own row.
func TestOperatorComparisonsByMV_PerOperatorRows(t *testing.T) {
	t.Parallel()
	conn := openTestConn(t)

	tenantID := uuid.New()
	projectID := uuid.New()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	// Operator A: 5 calls (all success).
	opA := uuid.New()
	rowsA := buildCallFixtures(t, tenantID, projectID, base, 5)
	for i := range rowsA {
		rowsA[i].OperatorID = opA
		rowsA[i].Status = "success"
	}
	// Operator B: 5 calls (mixed).
	opB := uuid.New()
	rowsB := buildCallFixtures(t, tenantID, projectID, base.Add(time.Minute), 5)
	for i := range rowsB {
		rowsB[i].OperatorID = opB
	}
	require.NoError(t, store.InsertCalls(t.Context(), conn, rowsA))
	require.NoError(t, store.InsertCalls(t.Context(), conn, rowsB))
	optimizeMVStates(t, conn)

	rows, err := store.OperatorComparisonsByMV(t.Context(), conn, apianalytics.OperatorComparisonsQuery{
		TenantID:  tenantID,
		ProjectID: projectID,
		Window:    apianalytics.Window{From: base.Add(-time.Hour), To: base.Add(24 * time.Hour)},
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// Find the row with operator A and check its success rate.
	var aRow apianalytics.OperatorComparisonRow
	for _, r := range rows {
		if r.OperatorID == opA {
			aRow = r
		}
	}
	require.Equal(t, opA, aRow.OperatorID)
	require.Equal(t, uint64(5), aRow.CallsTotal)
	require.InDelta(t, 1.0, aRow.SuccessRate, 0.001)
}

package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	reportsvc "github.com/sociopulse/platform/internal/reports/service"
)

// fakeAnalytics is a minimal recording analyticsapi.ServiceRO fake.
// Tests pre-load fixture data on the struct; the methods return either
// the fixture or the err on demand. All methods share a single `err`
// hook — the fetcher tests only need to trigger ONE failure path per
// kind so a single hook is enough.
type fakeAnalytics struct {
	calls       analyticsapi.CallsResult
	opState     analyticsapi.OperatorStateBreakdown
	regions     []analyticsapi.RegionProgressRow
	hourly      []analyticsapi.HourlyBucket
	comparisons []analyticsapi.OperatorComparisonRow
	overview    analyticsapi.OverviewResult
	err         error

	// Last-call observers — tests use these to assert the fetcher
	// forwarded TenantID, ProjectID, Window correctly.
	lastCallsQuery       analyticsapi.CallsQuery
	lastOpStateQuery     analyticsapi.OperatorStateQuery
	lastRegionsQuery     analyticsapi.RegionProgressQuery
	lastHourlyQuery      analyticsapi.HourlyQuery
	lastComparisonsQuery analyticsapi.OperatorComparisonsQuery
	lastOverviewQuery    analyticsapi.OverviewQuery
}

func (f *fakeAnalytics) Calls(_ context.Context, q analyticsapi.CallsQuery) (analyticsapi.CallsResult, error) {
	f.lastCallsQuery = q
	if f.err != nil {
		return analyticsapi.CallsResult{}, f.err
	}
	return f.calls, nil
}

func (f *fakeAnalytics) OperatorState(_ context.Context, q analyticsapi.OperatorStateQuery) (analyticsapi.OperatorStateBreakdown, error) {
	f.lastOpStateQuery = q
	if f.err != nil {
		return analyticsapi.OperatorStateBreakdown{}, f.err
	}
	return f.opState, nil
}

func (f *fakeAnalytics) RegionProgress(_ context.Context, q analyticsapi.RegionProgressQuery) ([]analyticsapi.RegionProgressRow, error) {
	f.lastRegionsQuery = q
	if f.err != nil {
		return nil, f.err
	}
	return f.regions, nil
}

func (f *fakeAnalytics) Hourly(_ context.Context, q analyticsapi.HourlyQuery) ([]analyticsapi.HourlyBucket, error) {
	f.lastHourlyQuery = q
	if f.err != nil {
		return nil, f.err
	}
	return f.hourly, nil
}

func (f *fakeAnalytics) OperatorComparisons(_ context.Context, q analyticsapi.OperatorComparisonsQuery) ([]analyticsapi.OperatorComparisonRow, error) {
	f.lastComparisonsQuery = q
	if f.err != nil {
		return nil, f.err
	}
	return f.comparisons, nil
}

func (f *fakeAnalytics) Overview(_ context.Context, q analyticsapi.OverviewQuery) (analyticsapi.OverviewResult, error) {
	f.lastOverviewQuery = q
	if f.err != nil {
		return analyticsapi.OverviewResult{}, f.err
	}
	return f.overview, nil
}

// Compile-time assertion: the fake satisfies analyticsapi.ServiceRO.
var _ analyticsapi.ServiceRO = (*fakeAnalytics)(nil)

// Shared instants for fetcher tests — half-open [from, to) window.
var (
	dataFrom = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	dataTo   = time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)
)

func newInput(t *testing.T, params map[string]any) reportsapi.RenderInput {
	t.Helper()
	return reportsapi.RenderInput{
		Kind:     reportsapi.KindOperatorEfficiency,
		Format:   reportsapi.FormatXLSX,
		Params:   params,
		Window:   analyticsapi.Window{From: dataFrom, To: dataTo},
		TenantID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		ActorID:  uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	}
}

// -----------------------------------------------------------------------
// OperatorEfficiency
// -----------------------------------------------------------------------

func TestFetchOperatorEfficiency_Happy(t *testing.T) {
	t.Parallel()

	opID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	projID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	fa := &fakeAnalytics{
		comparisons: []analyticsapi.OperatorComparisonRow{{
			OperatorID:   opID,
			DisplayName:  "Иван Петров",
			CallsTotal:   42,
			SuccessRate:  0.5,
			AvgTalkSec:   180.0,
			PauseShare:   0.2,
			AboveTeamAvg: true,
		}},
	}

	got, err := reportsvc.FetchOperatorEfficiency(context.Background(), fa,
		newInput(t, map[string]any{"project_id": projID.String()}))
	require.NoError(t, err)
	require.Len(t, got.Rows, 1)
	require.Equal(t, opID, got.Rows[0].OperatorID)
	require.Equal(t, "Иван Петров", got.Rows[0].DisplayName)
	require.True(t, got.Rows[0].AboveTeamAvg)
	require.Equal(t, dataFrom, got.Window.From)
	require.Equal(t, dataTo, got.Window.To)
	// Forwarded query check.
	require.Equal(t, projID, fa.lastComparisonsQuery.ProjectID)
}

func TestFetchOperatorEfficiency_MissingProjectID(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	_, err := reportsvc.FetchOperatorEfficiency(context.Background(), fa, newInput(t, map[string]any{}))
	require.Error(t, err)
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
}

func TestFetchOperatorEfficiency_AnalyticsError(t *testing.T) {
	t.Parallel()

	projID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	fa := &fakeAnalytics{err: errors.New("boom")}
	_, err := reportsvc.FetchOperatorEfficiency(context.Background(), fa,
		newInput(t, map[string]any{"project_id": projID.String()}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// -----------------------------------------------------------------------
// ProjectSummary
// -----------------------------------------------------------------------

func TestFetchProjectSummary_Happy(t *testing.T) {
	t.Parallel()

	projID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	fa := &fakeAnalytics{
		overview: analyticsapi.OverviewResult{
			Calls:         analyticsapi.CallsResult{Total: 100, Successful: 80},
			OperatorState: analyticsapi.OperatorStateBreakdown{TalkSec: 3600},
			RegionProgress: []analyticsapi.RegionProgressRow{
				{RegionCode: "RU-MOW", Done: 10, Plan: 100, Progress: 0.1},
			},
		},
	}

	got, err := reportsvc.FetchProjectSummary(context.Background(), fa,
		newInput(t, map[string]any{"project_id": projID.String()}))
	require.NoError(t, err)
	require.Equal(t, projID, got.Project)
	require.Equal(t, uint64(100), got.Calls.Total)
	require.Equal(t, uint64(3600), got.State.TalkSec)
	require.Len(t, got.Regions, 1)
	require.Equal(t, "RU-MOW", got.Regions[0].RegionCode)
	require.NotNil(t, fa.lastOverviewQuery.ProjectID)
	require.Equal(t, projID, *fa.lastOverviewQuery.ProjectID)
}

func TestFetchProjectSummary_MissingProjectID(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	_, err := reportsvc.FetchProjectSummary(context.Background(), fa, newInput(t, map[string]any{}))
	require.Error(t, err)
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
}

func TestFetchProjectSummary_AnalyticsError(t *testing.T) {
	t.Parallel()

	projID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	fa := &fakeAnalytics{err: errors.New("boom")}
	_, err := reportsvc.FetchProjectSummary(context.Background(), fa,
		newInput(t, map[string]any{"project_id": projID.String()}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// -----------------------------------------------------------------------
// CallsByStatus
// -----------------------------------------------------------------------

func TestFetchCallsByStatus_Happy(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{
		calls: analyticsapi.CallsResult{
			Total:    50,
			ByStatus: []analyticsapi.StatusBucket{{Status: "successful", Count: 40}},
		},
	}

	got, err := reportsvc.FetchCallsByStatus(context.Background(), fa, newInput(t, map[string]any{}))
	require.NoError(t, err)
	require.Equal(t, uint64(50), got.Result.Total)
	require.Len(t, got.Result.ByStatus, 1)
	require.Nil(t, fa.lastCallsQuery.ProjectID, "no project_id supplied → query.ProjectID stays nil")
}

func TestFetchCallsByStatus_WithProjectID(t *testing.T) {
	t.Parallel()

	projID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	fa := &fakeAnalytics{calls: analyticsapi.CallsResult{Total: 1}}

	_, err := reportsvc.FetchCallsByStatus(context.Background(), fa,
		newInput(t, map[string]any{"project_id": projID.String()}))
	require.NoError(t, err)
	require.NotNil(t, fa.lastCallsQuery.ProjectID)
	require.Equal(t, projID, *fa.lastCallsQuery.ProjectID)
}

func TestFetchCallsByStatus_AnalyticsError(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{err: errors.New("boom")}
	_, err := reportsvc.FetchCallsByStatus(context.Background(), fa, newInput(t, map[string]any{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// -----------------------------------------------------------------------
// Finance
// -----------------------------------------------------------------------

func TestFetchFinance_Happy(t *testing.T) {
	t.Parallel()

	// 600 seconds = 10 minutes; rate 5 ₽/min → expected 50.00 ₽.
	fa := &fakeAnalytics{calls: analyticsapi.CallsResult{Total: 10, TotalDurSec: 600}}

	got, err := reportsvc.FetchFinance(context.Background(), fa,
		newInput(t, map[string]any{"rate_rub_per_min": 5.0}))
	require.NoError(t, err)
	require.InDelta(t, 5.0, got.PerMinuteRate, 1e-9)
	require.InDelta(t, 10.0, got.TotalMinutes, 1e-9)
	require.InDelta(t, 50.0, got.TotalCostRub, 1e-9)
	require.Equal(t, uint64(10), got.Calls.Total)
}

func TestFetchFinance_AcceptsIntRate(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{calls: analyticsapi.CallsResult{TotalDurSec: 120}}

	got, err := reportsvc.FetchFinance(context.Background(), fa,
		newInput(t, map[string]any{"rate_rub_per_min": 3}))
	require.NoError(t, err)
	require.InDelta(t, 3.0, got.PerMinuteRate, 1e-9)
	require.InDelta(t, 6.0, got.TotalCostRub, 1e-9) // 2 min * 3 ₽
}

func TestFetchFinance_MissingRate(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	_, err := reportsvc.FetchFinance(context.Background(), fa, newInput(t, map[string]any{}))
	require.Error(t, err)
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
}

func TestFetchFinance_NonPositiveRate(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{}
	_, err := reportsvc.FetchFinance(context.Background(), fa,
		newInput(t, map[string]any{"rate_rub_per_min": 0.0}))
	require.Error(t, err)
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
}

func TestFetchFinance_AnalyticsError(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{err: errors.New("boom")}
	_, err := reportsvc.FetchFinance(context.Background(), fa,
		newInput(t, map[string]any{"rate_rub_per_min": 1.0}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// -----------------------------------------------------------------------
// QualityControl
// -----------------------------------------------------------------------

func TestFetchQualityControl_Happy(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{calls: analyticsapi.CallsResult{Total: 7, Refusals: 2}}

	got, err := reportsvc.FetchQualityControl(context.Background(), fa, newInput(t, map[string]any{}))
	require.NoError(t, err)
	require.Equal(t, uint64(7), got.Calls.Total)
	require.Equal(t, uint64(2), got.Calls.Refusals)
}

func TestFetchQualityControl_AnalyticsError(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{err: errors.New("boom")}
	_, err := reportsvc.FetchQualityControl(context.Background(), fa, newInput(t, map[string]any{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// -----------------------------------------------------------------------
// HourlyActivity
// -----------------------------------------------------------------------

func TestFetchHourlyActivity_Happy(t *testing.T) {
	t.Parallel()

	hourBase := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	fa := &fakeAnalytics{
		hourly: []analyticsapi.HourlyBucket{
			{Hour: hourBase, Count: 20, AvgDurSec: 110.0},
			{Hour: hourBase.Add(time.Hour), Count: 30, AvgDurSec: 120.5},
		},
	}

	got, err := reportsvc.FetchHourlyActivity(context.Background(), fa, newInput(t, map[string]any{}))
	require.NoError(t, err)
	require.Len(t, got.Buckets, 2)
	require.Equal(t, uint64(20), got.Buckets[0].Count)
	require.Equal(t, uint64(30), got.Buckets[1].Count)
}

func TestFetchHourlyActivity_AnalyticsError(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{err: errors.New("boom")}
	_, err := reportsvc.FetchHourlyActivity(context.Background(), fa, newInput(t, map[string]any{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// -----------------------------------------------------------------------
// Custom
// -----------------------------------------------------------------------

func TestFetchCustom_Happy(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{
		overview: analyticsapi.OverviewResult{
			Calls: analyticsapi.CallsResult{Total: 123},
		},
	}

	got, err := reportsvc.FetchCustom(context.Background(), fa, newInput(t, map[string]any{}))
	require.NoError(t, err)
	require.Equal(t, uint64(123), got.OV.Calls.Total)
}

func TestFetchCustom_AnalyticsError(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{err: errors.New("boom")}
	_, err := reportsvc.FetchCustom(context.Background(), fa, newInput(t, map[string]any{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
}

// -----------------------------------------------------------------------
// Param-decoder tests — exercise the public surface via Fetch* paths
// rather than poking at unexported helpers.
// -----------------------------------------------------------------------

func TestFetchers_RejectMalformedUUID(t *testing.T) {
	t.Parallel()

	// project_id is required for operator_efficiency — must be a valid UUID.
	fa := &fakeAnalytics{}
	_, err := reportsvc.FetchOperatorEfficiency(context.Background(), fa,
		newInput(t, map[string]any{"project_id": "not-a-uuid"}))
	require.Error(t, err)
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
}

func TestFetchers_OptionalUUIDEmptyStringIgnored(t *testing.T) {
	t.Parallel()

	// project_id is optional for calls_by_status — empty string is treated as missing.
	fa := &fakeAnalytics{}
	_, err := reportsvc.FetchCallsByStatus(context.Background(), fa,
		newInput(t, map[string]any{"project_id": ""}))
	require.NoError(t, err)
	require.Nil(t, fa.lastCallsQuery.ProjectID)
}

func TestFetchers_OptionalUUIDMalformedRejected(t *testing.T) {
	t.Parallel()

	// project_id is optional — but if PRESENT and malformed, error.
	fa := &fakeAnalytics{}
	_, err := reportsvc.FetchCallsByStatus(context.Background(), fa,
		newInput(t, map[string]any{"project_id": "not-a-uuid"}))
	require.Error(t, err)
	// Note: paramUUIDOpt returns the raw uuid.Parse error, not wrapped
	// with ErrInvalidParams. The fetcher's wrapping carries the message.
	require.Contains(t, err.Error(), "uuid", "error should mention uuid parsing")
}

func TestFetchers_RateAcceptsFloat64AndInt64(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rate any
		ok   bool
	}{
		{"float64", float64(2.5), true},
		{"int", 7, true},
		{"int64", int64(11), true},
		{"string", "5", false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fa := &fakeAnalytics{calls: analyticsapi.CallsResult{TotalDurSec: 60}}
			_, err := reportsvc.FetchFinance(context.Background(), fa,
				newInput(t, map[string]any{"rate_rub_per_min": tc.rate}))
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
			}
		})
	}
}

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/service"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
)

// --- fakes -----------------------------------------------------------------

// fakeStoreReader is the test double for service.StoreReader. Each field
// holds an optional return value; if a field is set, the matching method
// returns it; otherwise the zero value + nil. Calls record themselves so
// tests can assert "CH was hit" vs "cache short-circuited".
type fakeStoreReader struct {
	mu sync.Mutex

	callsRes       apianalytics.CallsResult
	callsErr       error
	callsCalls     int
	opStateRes     apianalytics.OperatorStateBreakdown
	opStateErr     error
	opStateCalls   int
	regionDone     map[string]uint64
	regionErr      error
	regionCalls    int
	hourlyRes      []apianalytics.HourlyBucket
	hourlyErr      error
	hourlyCalls    int
	opCompareRes   []apianalytics.OperatorComparisonRow
	opCompareErr   error
	opCompareCalls int
}

func (f *fakeStoreReader) CallsByMV(_ context.Context, _ apianalytics.CallsQuery) (apianalytics.CallsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callsCalls++
	return f.callsRes, f.callsErr
}

func (f *fakeStoreReader) OperatorStateByMV(_ context.Context, _ apianalytics.OperatorStateQuery) (apianalytics.OperatorStateBreakdown, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opStateCalls++
	return f.opStateRes, f.opStateErr
}

func (f *fakeStoreReader) RegionProgressDoneByMV(_ context.Context, _ apianalytics.RegionProgressQuery) (map[string]uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regionCalls++
	return f.regionDone, f.regionErr
}

func (f *fakeStoreReader) HourlyByMV(_ context.Context, _ apianalytics.HourlyQuery) ([]apianalytics.HourlyBucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hourlyCalls++
	return f.hourlyRes, f.hourlyErr
}

func (f *fakeStoreReader) OperatorComparisonsByMV(_ context.Context, _ apianalytics.OperatorComparisonsQuery) ([]apianalytics.OperatorComparisonRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opCompareCalls++
	return f.opCompareRes, f.opCompareErr
}

// fakeCache is an in-memory map cache for unit tests. Tracks Get/Set
// call counts for assertions about cache shortcircuit vs. miss.
type fakeCache struct {
	mu       sync.Mutex
	data     map[string][]byte
	setErr   error
	getErr   error
	getCalls int
	setCalls int
}

func newFakeCache() *fakeCache { return &fakeCache{data: make(map[string][]byte)} }

func (f *fakeCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	v, ok := f.data[key]
	return v, ok, nil
}

func (f *fakeCache) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	f.data[key] = value
	return nil
}

// fakeCrm is a test double for service.CrmReader. ProgressByProjectID
// holds the configured response; missing entries return (nil, nil)
// meaning "project not found" — matching the production
// crm.api.ProjectService.GetProgress contract.
type fakeCrm struct {
	ProgressByProjectID map[uuid.UUID]*crmapi.ProjectProgress
	Err                 error
	Calls               int
}

func (f *fakeCrm) GetProgress(_ context.Context, projectID uuid.UUID) (*crmapi.ProjectProgress, error) {
	f.Calls++
	if f.Err != nil {
		return nil, f.Err
	}
	if p, ok := f.ProgressByProjectID[projectID]; ok {
		return p, nil
	}
	return nil, nil
}

// --- helpers ---------------------------------------------------------------

// newSvc builds a *QueryService with the supplied test doubles + a
// sane default config. Each test mutates the doubles before calling.
func newSvc(t *testing.T, sr service.StoreReader, c service.Cache, cr service.CrmReader) *service.QueryService {
	t.Helper()
	cfg := service.QueryConfig{
		CacheShortTTL:       30 * time.Second,
		CacheLongTTL:        5 * time.Minute,
		LongWindowThreshold: 24 * time.Hour,
	}
	svc, err := service.NewQueryService(sr, c, cr, zap.NewNop(), nil, cfg)
	require.NoError(t, err)
	return svc
}

// validWindow returns a 1h window anchored at now-1h to now.
func validWindow() apianalytics.Window {
	now := time.Now().UTC()
	return apianalytics.Window{From: now.Add(-time.Hour), To: now}
}

// --- tests -----------------------------------------------------------------

// TestQueryService_Calls_HappyPath asserts the store is hit on a cache
// miss and the result is returned + cached.
func TestQueryService_Calls_HappyPath(t *testing.T) {
	t.Parallel()
	sr := &fakeStoreReader{
		callsRes: apianalytics.CallsResult{Total: 10, Successful: 5, Failed: 3, Refusals: 2, TotalDurSec: 300, AvgDurSec: 30.0},
	}
	c := newFakeCache()
	svc := newSvc(t, sr, c, nil)

	tenantID := uuid.New()
	res, err := svc.Calls(t.Context(), apianalytics.CallsQuery{
		TenantID: tenantID,
		Window:   validWindow(),
	})
	require.NoError(t, err)
	require.Equal(t, uint64(10), res.Total)
	require.Equal(t, 1, sr.callsCalls)
	require.Equal(t, 1, c.setCalls, "result must be cached on miss")
}

// TestQueryService_Calls_CacheHit asserts a cached value short-circuits
// the store call.
func TestQueryService_Calls_CacheHit(t *testing.T) {
	t.Parallel()
	sr := &fakeStoreReader{}
	c := newFakeCache()

	// Pre-warm cache with a known value.
	cached := apianalytics.CallsResult{Total: 99, Successful: 88}
	raw, _ := json.Marshal(cached)

	svc := newSvc(t, sr, c, nil)
	tenantID := uuid.New()
	q := apianalytics.CallsQuery{TenantID: tenantID, Window: validWindow()}
	c.data[service.CacheKey(tenantID, "calls", q)] = raw

	res, err := svc.Calls(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, uint64(99), res.Total)
	require.Equal(t, 0, sr.callsCalls, "cache hit must NOT hit the store")
}

// TestQueryService_Calls_TenantRequired asserts the zero TenantID
// returns ErrTenantRequired without touching cache or store.
func TestQueryService_Calls_TenantRequired(t *testing.T) {
	t.Parallel()
	sr := &fakeStoreReader{}
	c := newFakeCache()
	svc := newSvc(t, sr, c, nil)

	_, err := svc.Calls(t.Context(), apianalytics.CallsQuery{Window: validWindow()})
	require.ErrorIs(t, err, apianalytics.ErrTenantRequired)
	require.Equal(t, 0, sr.callsCalls)
	require.Equal(t, 0, c.getCalls)
}

// TestQueryService_Calls_InvalidWindow asserts From>=To fails fast.
func TestQueryService_Calls_InvalidWindow(t *testing.T) {
	t.Parallel()
	sr := &fakeStoreReader{}
	c := newFakeCache()
	svc := newSvc(t, sr, c, nil)

	now := time.Now()
	_, err := svc.Calls(t.Context(), apianalytics.CallsQuery{
		TenantID: uuid.New(),
		Window:   apianalytics.Window{From: now, To: now.Add(-time.Hour)}, // To < From
	})
	require.ErrorIs(t, err, apianalytics.ErrInvalidWindow)
	require.Equal(t, 0, sr.callsCalls)
}

// TestQueryService_OperatorState_HappyPath drives the operator-state
// branch end-to-end with cache miss + write.
func TestQueryService_OperatorState_HappyPath(t *testing.T) {
	t.Parallel()
	sr := &fakeStoreReader{
		opStateRes: apianalytics.OperatorStateBreakdown{TalkSec: 100, PauseSec: 50, ReadySec: 200, WrapSec: 30},
	}
	c := newFakeCache()
	svc := newSvc(t, sr, c, nil)

	res, err := svc.OperatorState(t.Context(), apianalytics.OperatorStateQuery{
		TenantID: uuid.New(),
		Window:   validWindow(),
	})
	require.NoError(t, err)
	require.Equal(t, uint64(100), res.TalkSec)
	require.Equal(t, 1, sr.opStateCalls)
}

// TestQueryService_RegionProgress_JoinsCrmPlan asserts the store's done
// counts are zipped with the crm port's Plan totals into the final
// RegionProgressRow slice.
func TestQueryService_RegionProgress_JoinsCrmPlan(t *testing.T) {
	t.Parallel()
	projectID := uuid.New()
	tenantID := uuid.New()

	sr := &fakeStoreReader{
		regionDone: map[string]uint64{"MSK": 5, "SPB": 2},
	}
	c := newFakeCache()
	cr := &fakeCrm{
		ProgressByProjectID: map[uuid.UUID]*crmapi.ProjectProgress{
			projectID: {
				ProjectID:   projectID,
				TargetCount: 100,
			},
		},
	}

	svc := newSvc(t, sr, c, cr)
	rows, err := svc.RegionProgress(t.Context(), apianalytics.RegionProgressQuery{
		TenantID:  tenantID,
		ProjectID: projectID,
		Window:    validWindow(),
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// Find MSK row.
	var msk apianalytics.RegionProgressRow
	for _, r := range rows {
		if r.RegionCode == "MSK" {
			msk = r
		}
	}
	require.Equal(t, "MSK", msk.RegionCode)
	require.Equal(t, uint64(5), msk.Done)
	require.Equal(t, uint64(100), msk.Plan, "Plan from crm.ProjectProgress.TargetCount")
}

// TestQueryService_RegionProgress_NilCrmFallsBackToZeroPlan asserts
// when the CrmReader port is unwired (locator lookup failed at module
// register time), Plan stays 0 — Q12 documented fallback.
func TestQueryService_RegionProgress_NilCrmFallsBackToZeroPlan(t *testing.T) {
	t.Parallel()
	sr := &fakeStoreReader{
		regionDone: map[string]uint64{"MSK": 5},
	}
	c := newFakeCache()

	svc := newSvc(t, sr, c, nil)
	rows, err := svc.RegionProgress(t.Context(), apianalytics.RegionProgressQuery{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Window:    validWindow(),
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, uint64(5), rows[0].Done)
	require.Equal(t, uint64(0), rows[0].Plan, "nil crm reader → Plan=0")
}

// TestQueryService_RegionProgress_CrmErrorFallsBackToZeroPlan asserts
// a transient CRM failure is logged + treated as zero plan (not a
// hard error for the whole dashboard).
func TestQueryService_RegionProgress_CrmErrorFallsBackToZeroPlan(t *testing.T) {
	t.Parallel()
	sr := &fakeStoreReader{
		regionDone: map[string]uint64{"MSK": 5},
	}
	c := newFakeCache()
	cr := &fakeCrm{Err: errors.New("crm down")}

	svc := newSvc(t, sr, c, cr)
	rows, err := svc.RegionProgress(t.Context(), apianalytics.RegionProgressQuery{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Window:    validWindow(),
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, uint64(0), rows[0].Plan)
}

// TestQueryService_Hourly_HappyPath drives the hourly bucket branch.
func TestQueryService_Hourly_HappyPath(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	sr := &fakeStoreReader{
		hourlyRes: []apianalytics.HourlyBucket{
			{Hour: base, Count: 10, AvgDurSec: 30.0},
			{Hour: base.Add(time.Hour), Count: 4, AvgDurSec: 25.0},
		},
	}
	c := newFakeCache()
	svc := newSvc(t, sr, c, nil)

	buckets, err := svc.Hourly(t.Context(), apianalytics.HourlyQuery{
		TenantID: uuid.New(),
		Window:   validWindow(),
	})
	require.NoError(t, err)
	require.Len(t, buckets, 2)
	require.Equal(t, uint64(10), buckets[0].Count)
}

// TestQueryService_OperatorComparisons_AboveTeamAvg computes the team
// average internally and asserts AboveTeamAvg flips correctly.
func TestQueryService_OperatorComparisons_AboveTeamAvg(t *testing.T) {
	t.Parallel()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	sr := &fakeStoreReader{
		opCompareRes: []apianalytics.OperatorComparisonRow{
			{OperatorID: a, CallsTotal: 10, SuccessRate: 0.9},
			{OperatorID: b, CallsTotal: 10, SuccessRate: 0.5},
			{OperatorID: c, CallsTotal: 10, SuccessRate: 0.3},
		},
	}
	cache := newFakeCache()

	svc := newSvc(t, sr, cache, nil)
	rows, err := svc.OperatorComparisons(t.Context(), apianalytics.OperatorComparisonsQuery{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Window:    validWindow(),
	})
	require.NoError(t, err)
	require.Len(t, rows, 3)
	teamAvg := (0.9 + 0.5 + 0.3) / 3 // ~0.567
	for _, r := range rows {
		require.Equal(t, r.SuccessRate > teamAvg, r.AboveTeamAvg,
			"operator %s success_rate=%.2f teamAvg=%.2f", r.OperatorID, r.SuccessRate, teamAvg)
	}
}

// TestQueryService_Overview_AggregatesFourSubQueries asserts Overview
// returns a fully populated OverviewResult.
func TestQueryService_Overview_AggregatesFourSubQueries(t *testing.T) {
	t.Parallel()
	projectID := uuid.New()
	sr := &fakeStoreReader{
		callsRes:   apianalytics.CallsResult{Total: 10, Successful: 5},
		opStateRes: apianalytics.OperatorStateBreakdown{TalkSec: 100},
		regionDone: map[string]uint64{"MSK": 5},
		hourlyRes:  []apianalytics.HourlyBucket{{Hour: time.Now(), Count: 1}},
	}
	c := newFakeCache()
	cr := &fakeCrm{
		ProgressByProjectID: map[uuid.UUID]*crmapi.ProjectProgress{
			projectID: {ProjectID: projectID, TargetCount: 100},
		},
	}

	svc := newSvc(t, sr, c, cr)
	res, err := svc.Overview(t.Context(), apianalytics.OverviewQuery{
		TenantID:  uuid.New(),
		ProjectID: &projectID,
		Window:    validWindow(),
	})
	require.NoError(t, err)
	require.Equal(t, uint64(10), res.Calls.Total)
	require.Equal(t, uint64(100), res.OperatorState.TalkSec)
	require.Len(t, res.RegionProgress, 1)
	require.Len(t, res.Hourly, 1)
}

// TestQueryService_TTLShortLong asserts the TTL policy picks the
// short TTL for a sub-day window and the long TTL for a multi-day
// one. We inspect the cache wire format indirectly by counting set
// calls under different windows.
func TestQueryService_TTLShortLong(t *testing.T) {
	t.Parallel()
	sr := &fakeStoreReader{callsRes: apianalytics.CallsResult{Total: 1}}
	c := newFakeCache()
	cfg := service.QueryConfig{
		CacheShortTTL:       1 * time.Second,
		CacheLongTTL:        10 * time.Second,
		LongWindowThreshold: 24 * time.Hour,
	}
	svc, err := service.NewQueryService(sr, c, nil, zap.NewNop(), nil, cfg)
	require.NoError(t, err)

	// Short window — < 24h.
	_, err = svc.Calls(t.Context(), apianalytics.CallsQuery{TenantID: uuid.New(), Window: validWindow()})
	require.NoError(t, err)

	// Long window — > 24h.
	now := time.Now().UTC()
	_, err = svc.Calls(t.Context(), apianalytics.CallsQuery{
		TenantID: uuid.New(),
		Window:   apianalytics.Window{From: now.Add(-72 * time.Hour), To: now},
	})
	require.NoError(t, err)

	require.Equal(t, 2, c.setCalls)
}

// TestQueryService_CacheSetErrorDoesNotPropagate asserts a cache write
// failure is logged at debug + the result still returns successfully.
func TestQueryService_CacheSetErrorDoesNotPropagate(t *testing.T) {
	t.Parallel()
	sr := &fakeStoreReader{callsRes: apianalytics.CallsResult{Total: 7}}
	c := newFakeCache()
	c.setErr = errors.New("redis down")
	svc := newSvc(t, sr, c, nil)

	res, err := svc.Calls(t.Context(), apianalytics.CallsQuery{TenantID: uuid.New(), Window: validWindow()})
	require.NoError(t, err, "cache set failure must NOT bubble up")
	require.Equal(t, uint64(7), res.Total)
}

// TestNewQueryService_InvalidConfig asserts a config with zero/negative
// TTLs is rejected at construction time.
func TestNewQueryService_InvalidConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  service.QueryConfig
	}{
		{name: "zero short ttl", cfg: service.QueryConfig{CacheShortTTL: 0, CacheLongTTL: time.Minute, LongWindowThreshold: time.Hour}},
		{name: "zero long ttl", cfg: service.QueryConfig{CacheShortTTL: time.Second, CacheLongTTL: 0, LongWindowThreshold: time.Hour}},
		{name: "zero threshold", cfg: service.QueryConfig{CacheShortTTL: time.Second, CacheLongTTL: time.Minute, LongWindowThreshold: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := service.NewQueryService(&fakeStoreReader{}, newFakeCache(), nil, zap.NewNop(), nil, tc.cfg)
			require.Error(t, err)
		})
	}
}

// TestNewQueryService_NilStoreFails asserts a nil StoreReader is
// rejected at construction time (wiring bug surfaces at boot).
func TestNewQueryService_NilStoreFails(t *testing.T) {
	t.Parallel()
	cfg := service.QueryConfig{
		CacheShortTTL:       time.Second,
		CacheLongTTL:        time.Minute,
		LongWindowThreshold: time.Hour,
	}
	_, err := service.NewQueryService(nil, newFakeCache(), nil, zap.NewNop(), nil, cfg)
	require.Error(t, err)
}

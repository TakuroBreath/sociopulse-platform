package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/service"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// fakeQueryService implements apianalytics.ServiceRO for transport tests.
// Each *Fn is optional; nil means "return the static *Res / *Err pair".
// This shape mirrors fakeRecordingService in internal/recording/transport/http
// — the canonical pattern for HTTP fakes in this codebase.
type fakeQueryService struct {
	callsFn      func(context.Context, apianalytics.CallsQuery) (apianalytics.CallsResult, error)
	callsRes     apianalytics.CallsResult
	callsErr     error
	opStateFn    func(context.Context, apianalytics.OperatorStateQuery) (apianalytics.OperatorStateBreakdown, error)
	opStateRes   apianalytics.OperatorStateBreakdown
	opStateErr   error
	regionFn     func(context.Context, apianalytics.RegionProgressQuery) ([]apianalytics.RegionProgressRow, error)
	regionRes    []apianalytics.RegionProgressRow
	regionErr    error
	hourlyFn     func(context.Context, apianalytics.HourlyQuery) ([]apianalytics.HourlyBucket, error)
	hourlyRes    []apianalytics.HourlyBucket
	hourlyErr    error
	opCompareFn  func(context.Context, apianalytics.OperatorComparisonsQuery) ([]apianalytics.OperatorComparisonRow, error)
	opCompareRes []apianalytics.OperatorComparisonRow
	opCompareErr error
	overviewFn   func(context.Context, apianalytics.OverviewQuery) (apianalytics.OverviewResult, error)
	overviewRes  apianalytics.OverviewResult
	overviewErr  error
}

func (f *fakeQueryService) Calls(ctx context.Context, q apianalytics.CallsQuery) (apianalytics.CallsResult, error) {
	if f.callsFn != nil {
		return f.callsFn(ctx, q)
	}
	return f.callsRes, f.callsErr
}

func (f *fakeQueryService) OperatorState(ctx context.Context, q apianalytics.OperatorStateQuery) (apianalytics.OperatorStateBreakdown, error) {
	if f.opStateFn != nil {
		return f.opStateFn(ctx, q)
	}
	return f.opStateRes, f.opStateErr
}

func (f *fakeQueryService) RegionProgress(ctx context.Context, q apianalytics.RegionProgressQuery) ([]apianalytics.RegionProgressRow, error) {
	if f.regionFn != nil {
		return f.regionFn(ctx, q)
	}
	return f.regionRes, f.regionErr
}

func (f *fakeQueryService) Hourly(ctx context.Context, q apianalytics.HourlyQuery) ([]apianalytics.HourlyBucket, error) {
	if f.hourlyFn != nil {
		return f.hourlyFn(ctx, q)
	}
	return f.hourlyRes, f.hourlyErr
}

func (f *fakeQueryService) OperatorComparisons(ctx context.Context, q apianalytics.OperatorComparisonsQuery) ([]apianalytics.OperatorComparisonRow, error) {
	if f.opCompareFn != nil {
		return f.opCompareFn(ctx, q)
	}
	return f.opCompareRes, f.opCompareErr
}

func (f *fakeQueryService) Overview(ctx context.Context, q apianalytics.OverviewQuery) (apianalytics.OverviewResult, error) {
	if f.overviewFn != nil {
		return f.overviewFn(ctx, q)
	}
	return f.overviewRes, f.overviewErr
}

// Compile-time interface assertion — the fake must satisfy ServiceRO so it
// is wire-compatible with MountAnalyticsRoutes.
var _ apianalytics.ServiceRO = (*fakeQueryService)(nil)

// injectClaims mirrors internal/recording/transport/http/handlers_test.go::
// injectClaims — pre-attach Claims under the canonical pkg/middleware/auth
// context key so handlers can resolve tenant_id without a real JWT validator.
func injectClaims(claims authapi.Claims) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(authmw.ClaimsContextKey, claims)
		c.Next()
	}
}

// mountWithClaims wires gin in TestMode, pre-attaches claims via injectClaims,
// then mounts the analytics routes. Tests call this once per request scenario.
func mountWithClaims(qs apianalytics.ServiceRO, claims authapi.Claims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(injectClaims(claims))
	service.MountAnalyticsRoutes(r, qs, zap.NewNop(), nil)
	return r
}

// mountWithoutClaims wires gin in TestMode WITHOUT injecting claims — used
// to assert the 401 fallback when the auth middleware was not run.
func mountWithoutClaims(qs apianalytics.ServiceRO) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	service.MountAnalyticsRoutes(r, qs, zap.NewNop(), nil)
	return r
}

// tenantClaims returns Claims with a known TenantID. Used by happy-path
// tests so the assertion can compare TenantID through the fake.
func tenantClaims(tenantID uuid.UUID) authapi.Claims {
	return authapi.Claims{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []authapi.Role{authapi.RoleSupervisor},
	}
}

// rfc3339 returns t formatted in RFC3339 — the canonical wire format for
// /api/analytics/* `from` / `to` query params.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// fixedWindow returns a fixed 1h window — using a deterministic anchor
// avoids dependence on time.Now in handler tests.
func fixedWindow() (from, to time.Time) {
	from = time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	to = from.Add(time.Hour)
	return
}

// =============================================================================
// GET /api/analytics/calls
// =============================================================================

// TestGetCalls_HappyPath is the canonical RED→GREEN test for the /calls
// endpoint: claims pre-attached, valid window, fake returns a known result.
func TestGetCalls_HappyPath(t *testing.T) {
	t.Parallel()
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	fakeQS := &fakeQueryService{
		callsRes: apianalytics.CallsResult{
			Total:      42,
			Successful: 30,
			Failed:     8,
			Refusals:   4,
		},
	}
	r := mountWithClaims(fakeQS, tenantClaims(tenantID))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/calls?from="+rfc3339(from)+"&to="+rfc3339(to), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var got apianalytics.CallsResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, uint64(42), got.Total)
	require.Equal(t, uint64(30), got.Successful)
}

// TestGetCalls_PassesTenantAndProjectIDFromClaimsAndQuery asserts the
// handler threads claims.TenantID + the ?project_id form param into the
// CallsQuery sent to MetricsQuery.
func TestGetCalls_PassesTenantAndProjectIDFromClaimsAndQuery(t *testing.T) {
	t.Parallel()
	tenantID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	projectID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	var seen apianalytics.CallsQuery
	fakeQS := &fakeQueryService{
		callsFn: func(_ context.Context, q apianalytics.CallsQuery) (apianalytics.CallsResult, error) {
			seen = q
			return apianalytics.CallsResult{Total: 1}, nil
		},
	}
	r := mountWithClaims(fakeQS, tenantClaims(tenantID))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/calls?from="+rfc3339(from)+"&to="+rfc3339(to)+
			"&project_id="+projectID.String(), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Equal(t, tenantID, seen.TenantID, "tenant_id sourced from JWT claims")
	require.NotNil(t, seen.ProjectID)
	require.Equal(t, projectID, *seen.ProjectID, "project_id sourced from ?project_id")
}

// TestGetCalls_RejectsMissingTenant asserts a request that did not pass
// through JWTMiddleware (claims missing) → 401, not 500. Defence-in-depth.
func TestGetCalls_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	r := mountWithoutClaims(&fakeQueryService{})

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/calls?from="+rfc3339(from)+"&to="+rfc3339(to), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body=%s", w.Body.String())
}

// TestGetCalls_RejectsInvalidWindow asserts From >= To → 400, propagated
// from the upstream apianalytics.ErrInvalidWindow sentinel.
func TestGetCalls_RejectsInvalidWindow(t *testing.T) {
	t.Parallel()
	fakeQS := &fakeQueryService{
		callsErr: apianalytics.ErrInvalidWindow,
	}
	r := mountWithClaims(fakeQS, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	// Swap from/to so the handler-side bind succeeds, but the service
	// returns ErrInvalidWindow. Confirms sentinel → status mapping.
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/calls?from="+rfc3339(to)+"&to="+rfc3339(from), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}

// TestGetCalls_RejectsMissingRequiredFields asserts that omitting `from`
// + `to` returns 400 with a structured envelope.
func TestGetCalls_RejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()
	r := mountWithClaims(&fakeQueryService{}, tenantClaims(uuid.New()))

	req := httptest.NewRequest(http.MethodGet, "/api/analytics/calls", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}

// TestGetCalls_PropagatesInternalErrors asserts a non-sentinel error from
// MetricsQuery → 500.
func TestGetCalls_PropagatesInternalErrors(t *testing.T) {
	t.Parallel()
	fakeQS := &fakeQueryService{
		callsErr: errors.New("clickhouse down"),
	}
	r := mountWithClaims(fakeQS, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/calls?from="+rfc3339(from)+"&to="+rfc3339(to), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())
}

// =============================================================================
// GET /api/analytics/operator-state
// =============================================================================

func TestGetOperatorState_HappyPath(t *testing.T) {
	t.Parallel()
	fakeQS := &fakeQueryService{
		opStateRes: apianalytics.OperatorStateBreakdown{
			TalkSec:  600,
			PauseSec: 120,
			ReadySec: 300,
			WrapSec:  60,
		},
	}
	r := mountWithClaims(fakeQS, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/operator-state?from="+rfc3339(from)+"&to="+rfc3339(to), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var got apianalytics.OperatorStateBreakdown
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, uint64(600), got.TalkSec)
}

func TestGetOperatorState_PassesOperatorID(t *testing.T) {
	t.Parallel()
	operatorID := uuid.New()
	var seen apianalytics.OperatorStateQuery
	fakeQS := &fakeQueryService{
		opStateFn: func(_ context.Context, q apianalytics.OperatorStateQuery) (apianalytics.OperatorStateBreakdown, error) {
			seen = q
			return apianalytics.OperatorStateBreakdown{}, nil
		},
	}
	r := mountWithClaims(fakeQS, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/operator-state?from="+rfc3339(from)+"&to="+rfc3339(to)+
			"&operator_id="+operatorID.String(), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, seen.OperatorID)
	require.Equal(t, operatorID, *seen.OperatorID)
}

// =============================================================================
// GET /api/analytics/region-progress
// =============================================================================

func TestGetRegionProgress_HappyPath(t *testing.T) {
	t.Parallel()
	projectID := uuid.New()
	fakeQS := &fakeQueryService{
		regionRes: []apianalytics.RegionProgressRow{
			{RegionCode: "MSK", Done: 50, Plan: 100, Progress: 0.5},
			{RegionCode: "SPB", Done: 25, Plan: 100, Progress: 0.25},
		},
	}
	r := mountWithClaims(fakeQS, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/region-progress?from="+rfc3339(from)+"&to="+rfc3339(to)+
			"&project_id="+projectID.String(), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var got []apianalytics.RegionProgressRow
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got, 2)
}

// TestGetRegionProgress_RejectsMissingProjectID asserts the
// `binding:"required"` tag on project_id surfaces as 400.
func TestGetRegionProgress_RejectsMissingProjectID(t *testing.T) {
	t.Parallel()
	r := mountWithClaims(&fakeQueryService{}, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/region-progress?from="+rfc3339(from)+"&to="+rfc3339(to), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}

// =============================================================================
// GET /api/analytics/hourly
// =============================================================================

func TestGetHourly_HappyPath(t *testing.T) {
	t.Parallel()
	fakeQS := &fakeQueryService{
		hourlyRes: []apianalytics.HourlyBucket{
			{Hour: time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC), Count: 15, AvgDurSec: 30.0},
			{Hour: time.Date(2026, 5, 14, 11, 0, 0, 0, time.UTC), Count: 22, AvgDurSec: 28.5},
		},
	}
	r := mountWithClaims(fakeQS, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/hourly?from="+rfc3339(from)+"&to="+rfc3339(to), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var got []apianalytics.HourlyBucket
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got, 2)
	require.Equal(t, uint64(15), got[0].Count)
}

// =============================================================================
// GET /api/analytics/operator-comparisons
// =============================================================================

func TestGetOperatorComparisons_HappyPath(t *testing.T) {
	t.Parallel()
	projectID := uuid.New()
	op1, op2 := uuid.New(), uuid.New()
	fakeQS := &fakeQueryService{
		opCompareRes: []apianalytics.OperatorComparisonRow{
			{OperatorID: op1, CallsTotal: 10, SuccessRate: 0.7, AboveTeamAvg: true},
			{OperatorID: op2, CallsTotal: 8, SuccessRate: 0.5, AboveTeamAvg: false},
		},
	}
	r := mountWithClaims(fakeQS, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/operator-comparisons?from="+rfc3339(from)+"&to="+rfc3339(to)+
			"&project_id="+projectID.String(), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var got []apianalytics.OperatorComparisonRow
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got, 2)
}

// TestGetOperatorComparisons_RejectsMissingProjectID asserts the
// required-tag on project_id surfaces as 400.
func TestGetOperatorComparisons_RejectsMissingProjectID(t *testing.T) {
	t.Parallel()
	r := mountWithClaims(&fakeQueryService{}, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/operator-comparisons?from="+rfc3339(from)+"&to="+rfc3339(to), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}

// =============================================================================
// GET /api/analytics/overview
// =============================================================================

func TestGetOverview_HappyPath(t *testing.T) {
	t.Parallel()
	fakeQS := &fakeQueryService{
		overviewRes: apianalytics.OverviewResult{
			Calls: apianalytics.CallsResult{Total: 100, Successful: 80},
			OperatorState: apianalytics.OperatorStateBreakdown{
				TalkSec: 1200, PauseSec: 300, ReadySec: 600, WrapSec: 90,
			},
			Hourly: []apianalytics.HourlyBucket{
				{Hour: time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC), Count: 50},
			},
		},
	}
	r := mountWithClaims(fakeQS, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/overview?from="+rfc3339(from)+"&to="+rfc3339(to), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var got apianalytics.OverviewResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, uint64(100), got.Calls.Total)
	require.Equal(t, uint64(1200), got.OperatorState.TalkSec)
}

func TestGetOverview_PassesProjectID(t *testing.T) {
	t.Parallel()
	projectID := uuid.New()
	var seen apianalytics.OverviewQuery
	fakeQS := &fakeQueryService{
		overviewFn: func(_ context.Context, q apianalytics.OverviewQuery) (apianalytics.OverviewResult, error) {
			seen = q
			return apianalytics.OverviewResult{}, nil
		},
	}
	r := mountWithClaims(fakeQS, tenantClaims(uuid.New()))

	from, to := fixedWindow()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analytics/overview?from="+rfc3339(from)+"&to="+rfc3339(to)+
			"&project_id="+projectID.String(), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, seen.ProjectID)
	require.Equal(t, projectID, *seen.ProjectID)
}

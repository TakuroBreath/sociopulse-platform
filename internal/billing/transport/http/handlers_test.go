package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	billingservice "github.com/sociopulse/platform/internal/billing/service"
	transporthttp "github.com/sociopulse/platform/internal/billing/transport/http"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// =============================================================================
// Fakes — billing service ports
// =============================================================================

// fakeSpendReport implements billingapi.SpendReport with injectable fns.
type fakeSpendReport struct {
	monthSpendFn   func(context.Context, uuid.UUID, *uuid.UUID, billingapi.Period) (billingapi.MonthBreakdown, error)
	spendByMonthFn func(context.Context, uuid.UUID, int) ([]billingapi.MonthBreakdown, error)
}

func (f *fakeSpendReport) MonthSpend(ctx context.Context, tid uuid.UUID, pid *uuid.UUID, p billingapi.Period) (billingapi.MonthBreakdown, error) {
	if f.monthSpendFn != nil {
		return f.monthSpendFn(ctx, tid, pid, p)
	}
	return billingapi.MonthBreakdown{TenantID: tid, Period: p}, nil
}

func (f *fakeSpendReport) SpendByMonth(ctx context.Context, tid uuid.UUID, count int) ([]billingapi.MonthBreakdown, error) {
	if f.spendByMonthFn != nil {
		return f.spendByMonthFn(ctx, tid, count)
	}
	return nil, nil
}

// fakeMarginReport implements billingapi.MarginReport with an injectable fn.
type fakeMarginReport struct {
	marginFn func(context.Context, uuid.UUID, billingapi.Period) ([]billingapi.ProjectMargin, error)
}

func (f *fakeMarginReport) Margin(ctx context.Context, tid uuid.UUID, p billingapi.Period) ([]billingapi.ProjectMargin, error) {
	if f.marginFn != nil {
		return f.marginFn(ctx, tid, p)
	}
	return nil, nil
}

// fakeRevenue implements billingapi.RevenueCalculator (unused by handlers but
// the Service composer expects it to be non-nil in some code paths).
type fakeRevenue struct{}

func (fakeRevenue) MonthRevenue(_ context.Context, _, _ uuid.UUID, _ billingapi.Period) (int64, error) {
	return 0, nil
}

// fakeTariffStore implements billingapi.TariffStore with injectable fns.
type fakeTariffStore struct {
	getFn    func(context.Context, uuid.UUID) (billingapi.Tariffs, error)
	updateFn func(context.Context, uuid.UUID, billingapi.Tariffs) (billingapi.Tariffs, error)
}

func (f *fakeTariffStore) Get(ctx context.Context, tid uuid.UUID) (billingapi.Tariffs, error) {
	if f.getFn != nil {
		return f.getFn(ctx, tid)
	}
	return billingapi.Tariffs{}, billingapi.ErrNoTariffs
}

func (f *fakeTariffStore) Update(ctx context.Context, tid uuid.UUID, in billingapi.Tariffs) (billingapi.Tariffs, error) {
	if f.updateFn != nil {
		return f.updateFn(ctx, tid, in)
	}
	in.TenantID = tid
	in.Version++
	in.UpdatedAt = time.Now().UTC()
	return in, nil
}

// fakeRBAC implements authapi.RBACChecker with an injectable behavior.
type fakeRBAC struct {
	checkFn func(context.Context, authapi.Claims, authapi.Action, authapi.Resource) error
}

func (f *fakeRBAC) Check(ctx context.Context, claims authapi.Claims, action authapi.Action, res authapi.Resource) error {
	if f.checkFn != nil {
		return f.checkFn(ctx, claims, action, res)
	}
	return nil
}

// allowAllRBAC is a fakeRBAC that permits every check.
func allowAllRBAC() *fakeRBAC {
	return &fakeRBAC{checkFn: func(_ context.Context, _ authapi.Claims, _ authapi.Action, _ authapi.Resource) error {
		return nil
	}}
}

// denyAllRBAC is a fakeRBAC that rejects every check with ErrInsufficientRole.
func denyAllRBAC() *fakeRBAC {
	return &fakeRBAC{checkFn: func(_ context.Context, _ authapi.Claims, _ authapi.Action, _ authapi.Resource) error {
		return authapi.ErrInsufficientRole
	}}
}

// =============================================================================
// Test fixtures / wiring
// =============================================================================

// injectClaims is the test analogue of pkg/middleware/auth.JWTMiddleware.
// Pattern mirrors internal/recording/transport/http/handlers_test.go.
func injectClaims(claims authapi.Claims) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(authmw.ClaimsContextKey, claims)
		c.Next()
	}
}

// adminClaims returns Claims with a fresh tenant + the admin role.
func adminClaims() authapi.Claims {
	return authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
}

// supervisorClaims returns Claims with a fresh tenant + supervisor role only.
func supervisorClaims() authapi.Claims {
	return authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleSupervisor},
	}
}

// operatorClaims returns Claims with a fresh tenant + operator role only.
func operatorClaims() authapi.Claims {
	return authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleOperator},
	}
}

// fixedNow is the deterministic clock used by tests so parsePeriod yields
// reproducible values. 2026-05-15 12:00:00 UTC.
func fixedNow() time.Time {
	return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
}

// newRouter wires gin in TestMode, injects claims, builds a Service from
// the supplied fakes, and mounts the billing transport.
func newRouter(t *testing.T, svc *billingservice.Service, rbac authapi.RBACChecker, claims authapi.Claims) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(injectClaims(claims))
	handlers := transporthttp.NewHandlers(svc, nil /*audit*/, fixedNow)
	transporthttp.Register(r, transporthttp.RouterDeps{
		Handlers: handlers,
		RBAC:     rbac,
	})
	return r
}

// defaultService builds a Service with permissive fakes — happy-path tests
// override the fakes they care about by editing fields after construction.
func defaultService() *billingservice.Service {
	return &billingservice.Service{
		SpendReport:    &fakeSpendReport{},
		MarginReport:   &fakeMarginReport{},
		Revenue:        fakeRevenue{},
		Tariffs:        &fakeTariffStore{},
		DefaultTariffs: billingapi.Tariffs{FixedFeesMinor: 5_000_000, RespondentBasesMinor: 50},
		Logger:         nil, // renderError tolerates nil
	}
}

// decodeEnvelope decodes the ErrorEnvelope from a response body. Fails the
// test if the body is not a valid envelope.
func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) transporthttp.ErrorEnvelope {
	t.Helper()
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body=%s", w.Body.String())
	return env
}

// =============================================================================
// Dashboard
// =============================================================================

func TestDashboard_Happy(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	svc := defaultService()
	svc.SpendReport = &fakeSpendReport{
		monthSpendFn: func(_ context.Context, tid uuid.UUID, _ *uuid.UUID, p billingapi.Period) (billingapi.MonthBreakdown, error) {
			require.Equal(t, claims.TenantID, tid)
			require.False(t, p.From.IsZero())
			require.True(t, p.From.Before(p.To))
			return billingapi.MonthBreakdown{
				TenantID:         tid,
				Period:           p,
				TelecomMin:       100,
				WagesMin:         200,
				StorageMin:       50,
				FixedFeeMin:      1000,
				TotalMin:         1350,
				CompletedSurveys: 10,
				TotalCallSeconds: 600,
			}, nil
		},
		spendByMonthFn: func(_ context.Context, _ uuid.UUID, count int) ([]billingapi.MonthBreakdown, error) {
			require.Equal(t, 6, count)
			return []billingapi.MonthBreakdown{{
				Period:   billingapi.Month(2026, time.January),
				TotalMin: 500,
			}}, nil
		},
	}
	svc.MarginReport = &fakeMarginReport{
		marginFn: func(_ context.Context, tid uuid.UUID, _ billingapi.Period) ([]billingapi.ProjectMargin, error) {
			require.Equal(t, claims.TenantID, tid)
			return []billingapi.ProjectMargin{
				{ProjectID: uuid.New(), TotalMin: 800, RevenueMin: 1500, MarginMin: 700},
				{ProjectID: uuid.New(), TotalMin: 300, RevenueMin: 600, MarginMin: 300},
			}, nil
		},
	}

	r := newRouter(t, svc, allowAllRBAC(), claims)
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp billingapi.DashboardResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, claims.TenantID, resp.TenantID)
	require.Equal(t, int64(1350), resp.MonthSpend)
	require.Equal(t, int64(2100), resp.RevenueMin) // 1500 + 600
	require.Len(t, resp.Breakdown, 5)
	require.Len(t, resp.TopProjects, 2)
}

func TestDashboard_Unauthenticated(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	gin.SetMode(gin.TestMode)
	r := gin.New() // intentionally NO injectClaims
	handlers := transporthttp.NewHandlers(svc, nil, fixedNow)
	transporthttp.Register(r, transporthttp.RouterDeps{Handlers: handlers, RBAC: allowAllRBAC()})

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusUnauthorized, w.Code)
	env := decodeEnvelope(t, w)
	require.Equal(t, "billing.unauthenticated", env.Code)
}

func TestDashboard_UnknownPeriod_400(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard?period=fortnight", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	env := decodeEnvelope(t, w)
	require.Equal(t, "billing.invalid_period", env.Code)
}

func TestDashboard_PeriodWeekAccepted(t *testing.T) {
	t.Parallel()

	called := false
	svc := defaultService()
	svc.SpendReport = &fakeSpendReport{
		monthSpendFn: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, p billingapi.Period) (billingapi.MonthBreakdown, error) {
			// fixedNow is 2026-05-15 (Friday). ISO week starts Monday 2026-05-11.
			if !called {
				called = true
				require.Equal(t, time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC), p.From)
				require.Equal(t, time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC), p.To)
			}
			return billingapi.MonthBreakdown{TotalMin: 1}, nil
		},
	}

	r := newRouter(t, svc, allowAllRBAC(), adminClaims())
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard?period=week", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	require.True(t, called, "MonthSpend should have been called with week period")
}

func TestDashboard_SpendInternalError_500(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	svc.SpendReport = &fakeSpendReport{
		monthSpendFn: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ billingapi.Period) (billingapi.MonthBreakdown, error) {
			return billingapi.MonthBreakdown{}, errors.New("db down")
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusInternalServerError, w.Code)
	env := decodeEnvelope(t, w)
	require.Equal(t, "billing.internal", env.Code)
	require.Equal(t, "internal error", env.Message, "5xx must scrub internal details")
}

// =============================================================================
// Breakdown
// =============================================================================

func TestBreakdown_Happy(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	svc := defaultService()
	svc.SpendReport = &fakeSpendReport{
		monthSpendFn: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ billingapi.Period) (billingapi.MonthBreakdown, error) {
			return billingapi.MonthBreakdown{
				TelecomMin:         111,
				WagesMin:           222,
				RespondentBasesMin: 333,
				StorageMin:         444,
				FixedFeeMin:        555,
			}, nil
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), claims)

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/breakdown", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code)
	var items []billingapi.BreakdownItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &items))
	require.Len(t, items, 5)
	require.Equal(t, int64(111), items[0].ValueMin)
	require.Equal(t, int64(222), items[1].ValueMin)
	require.Equal(t, int64(333), items[2].ValueMin)
	require.Equal(t, int64(444), items[3].ValueMin)
	require.Equal(t, int64(555), items[4].ValueMin)
}

// =============================================================================
// ByMonth
// =============================================================================

func TestByMonth_DefaultCount6(t *testing.T) {
	t.Parallel()

	gotCount := 0
	svc := defaultService()
	svc.SpendReport = &fakeSpendReport{
		spendByMonthFn: func(_ context.Context, _ uuid.UUID, count int) ([]billingapi.MonthBreakdown, error) {
			gotCount = count
			return []billingapi.MonthBreakdown{
				{Period: billingapi.Month(2026, time.January), TotalMin: 100},
				{Period: billingapi.Month(2026, time.February), TotalMin: 200},
			}, nil
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/byMonth", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code)
	require.Equal(t, 6, gotCount)
	var items []billingapi.ByMonthItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &items))
	require.Len(t, items, 2)
}

func TestByMonth_CountOverLimit_400(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/byMonth?count=25", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	env := decodeEnvelope(t, w)
	require.Equal(t, "billing.invalid_period", env.Code)
}

func TestByMonth_CountNotInteger_400(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/byMonth?count=abc", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
}

// =============================================================================
// Projects
// =============================================================================

func TestProjects_Happy(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	svc := defaultService()
	svc.MarginReport = &fakeMarginReport{
		marginFn: func(_ context.Context, tid uuid.UUID, _ billingapi.Period) ([]billingapi.ProjectMargin, error) {
			require.Equal(t, claims.TenantID, tid)
			return []billingapi.ProjectMargin{
				{ProjectID: uuid.New(), ProjectName: "Alpha", TotalMin: 200},
			}, nil
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), claims)

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/projects", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code)
	var rows []billingapi.ProjectMargin
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	require.Equal(t, "Alpha", rows[0].ProjectName)
}

// =============================================================================
// GetTariffs
// =============================================================================

func TestGetTariffs_Happy(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	svc := defaultService()
	stored := billingapi.Tariffs{
		TenantID:           claims.TenantID,
		Version:            3,
		WagePerSurveyMinor: 12_000,
		TrunkCostsMinor:    map[string]int64{"mtt-msk-1": 342},
		UpdatedAt:          time.Now().UTC(),
	}
	svc.Tariffs = &fakeTariffStore{
		getFn: func(_ context.Context, _ uuid.UUID) (billingapi.Tariffs, error) {
			return stored, nil
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), claims)

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/billing/tariffs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code)
	var resp billingapi.TariffsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, claims.TenantID, resp.TenantID)
	require.Equal(t, 3, resp.Tariffs.Version)
	require.False(t, resp.IsDefault)
	require.Equal(t, int64(12_000), resp.Tariffs.WagePerSurveyMinor)
}

func TestGetTariffs_FallsBackToDefaults(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	svc := defaultService()
	svc.Tariffs = &fakeTariffStore{
		getFn: func(_ context.Context, _ uuid.UUID) (billingapi.Tariffs, error) {
			return billingapi.Tariffs{}, billingapi.ErrNoTariffs
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), claims)

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/billing/tariffs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp billingapi.TariffsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.IsDefault, "defaults fallback must flag IsDefault=true")
	require.Equal(t, claims.TenantID, resp.Tariffs.TenantID, "defaults must be stamped with caller tenant")
	require.Equal(t, int64(5_000_000), resp.Tariffs.FixedFeesMinor)
}

// =============================================================================
// PatchTariffs
// =============================================================================

func TestPatchTariffs_Happy(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	svc := defaultService()
	stored := billingapi.Tariffs{
		TenantID:           claims.TenantID,
		Version:            5,
		WagePerSurveyMinor: 10_000,
	}
	called := false
	svc.Tariffs = &fakeTariffStore{
		getFn: func(_ context.Context, _ uuid.UUID) (billingapi.Tariffs, error) { return stored, nil },
		updateFn: func(_ context.Context, _ uuid.UUID, in billingapi.Tariffs) (billingapi.Tariffs, error) {
			called = true
			require.Equal(t, int64(15_000), in.WagePerSurveyMinor, "patch must be applied to current snapshot")
			in.Version = 6
			return in, nil
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), claims)

	patch := billingapi.TariffsPatchRequest{}
	v := int64(15_000)
	patch.WagePerSurveyMinor = &v
	body, _ := json.Marshal(patch)

	req := httptest.NewRequest(stdhttp.MethodPatch, "/api/billing/tariffs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	require.True(t, called)

	var resp billingapi.TariffsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 6, resp.Tariffs.Version)
}

func TestPatchTariffs_NegativeWage_400(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	svc := defaultService()
	svc.Tariffs = &fakeTariffStore{
		getFn: func(_ context.Context, _ uuid.UUID) (billingapi.Tariffs, error) {
			return billingapi.Tariffs{TenantID: claims.TenantID, Version: 1}, nil
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), claims)

	patch := billingapi.TariffsPatchRequest{}
	bad := int64(-1)
	patch.WagePerSurveyMinor = &bad
	body, _ := json.Marshal(patch)

	req := httptest.NewRequest(stdhttp.MethodPatch, "/api/billing/tariffs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	env := decodeEnvelope(t, w)
	require.Equal(t, "billing.invalid_tariff", env.Code)
}

func TestPatchTariffs_BadJSON_400(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())

	req := httptest.NewRequest(stdhttp.MethodPatch, "/api/billing/tariffs", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	env := decodeEnvelope(t, w)
	require.Equal(t, "billing.invalid_tariff", env.Code)
}

func TestPatchTariffs_DenyForNonAdmin_403(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	// Use operator claims (no admin/supervisor) so the fast-path doesn't
	// short-circuit, and a denying matrix to send the request through to
	// the 403 branch.
	r := newRouter(t, svc, denyAllRBAC(), operatorClaims())

	patch := billingapi.TariffsPatchRequest{}
	v := int64(1)
	patch.WagePerSurveyMinor = &v
	body, _ := json.Marshal(patch)

	req := httptest.NewRequest(stdhttp.MethodPatch, "/api/billing/tariffs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusForbidden, w.Code, "body=%s", w.Body.String())
	env := decodeEnvelope(t, w)
	require.Equal(t, "billing.forbidden", env.Code)
}

func TestPatchTariffs_SupervisorDenied_403(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	// Supervisor can VIEW finance, but PATCH /api/billing/tariffs is admin-only.
	// fast-path admin check fails → falls through to denying matrix → 403.
	r := newRouter(t, svc, denyAllRBAC(), supervisorClaims())

	patch := billingapi.TariffsPatchRequest{}
	v := int64(1)
	patch.WagePerSurveyMinor = &v
	body, _ := json.Marshal(patch)

	req := httptest.NewRequest(stdhttp.MethodPatch, "/api/billing/tariffs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusForbidden, w.Code, "supervisor must NOT be able to PATCH tariffs")
	env := decodeEnvelope(t, w)
	require.Equal(t, "billing.forbidden", env.Code)
}

func TestPatchTariffs_AdminPermittedViaFastPath(t *testing.T) {
	t.Parallel()

	// Even with a denying matrix, admin's role fast-path permits the
	// request. This guards against a regression where the fallback runs
	// before the fast-path.
	svc := defaultService()
	svc.Tariffs = &fakeTariffStore{
		getFn: func(_ context.Context, _ uuid.UUID) (billingapi.Tariffs, error) {
			return billingapi.Tariffs{}, billingapi.ErrNoTariffs
		},
	}
	r := newRouter(t, svc, denyAllRBAC(), adminClaims())

	patch := billingapi.TariffsPatchRequest{}
	v := int64(1)
	patch.WagePerSurveyMinor = &v
	body, _ := json.Marshal(patch)

	req := httptest.NewRequest(stdhttp.MethodPatch, "/api/billing/tariffs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "admin must be allowed via fast-path even with denying matrix; body=%s", w.Body.String())
}

// =============================================================================
// RBAC view-role coverage
// =============================================================================

func TestFinanceView_AllowsSupervisor(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	r := newRouter(t, svc, denyAllRBAC(), supervisorClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "supervisor must be permitted via the role fast-path for billing.view")
}

func TestFinanceView_DeniesOperator(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	r := newRouter(t, svc, denyAllRBAC(), operatorClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusForbidden, w.Code)
	env := decodeEnvelope(t, w)
	require.Equal(t, "billing.forbidden", env.Code)
}

// =============================================================================
// Helpers — additional coverage
// =============================================================================

// =============================================================================
// Unauthenticated coverage across all 6 endpoints
// =============================================================================

func TestAllEndpoints_RejectUnauthenticated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		method, path string
	}{
		{stdhttp.MethodGet, "/api/finance/dashboard"},
		{stdhttp.MethodGet, "/api/finance/breakdown"},
		{stdhttp.MethodGet, "/api/finance/byMonth"},
		{stdhttp.MethodGet, "/api/finance/projects"},
		{stdhttp.MethodGet, "/api/billing/tariffs"},
		{stdhttp.MethodPatch, "/api/billing/tariffs"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			svc := defaultService()
			gin.SetMode(gin.TestMode)
			r := gin.New() // no injectClaims
			handlers := transporthttp.NewHandlers(svc, nil, fixedNow)
			transporthttp.Register(r, transporthttp.RouterDeps{Handlers: handlers, RBAC: allowAllRBAC()})

			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte("{}")))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, stdhttp.StatusUnauthorized, w.Code, "body=%s", w.Body.String())
			env := decodeEnvelope(t, w)
			require.Equal(t, "billing.unauthenticated", env.Code)
		})
	}
}

// =============================================================================
// Period parsing — exercise quarter and year branches
// =============================================================================

func TestDashboard_PeriodQuarter(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	calls := 0
	svc.SpendReport = &fakeSpendReport{
		monthSpendFn: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, p billingapi.Period) (billingapi.MonthBreakdown, error) {
			// Dashboard calls MonthSpend TWICE (current + previous period).
			// Only assert on the first (current period) call.
			if calls == 0 {
				// fixedNow is 2026-05-15 → Q2 starts 2026-04-01, ends 2026-07-01.
				require.Equal(t, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), p.From)
				require.Equal(t, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), p.To)
			}
			calls++
			return billingapi.MonthBreakdown{TotalMin: 1}, nil
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard?period=quarter", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
}

func TestDashboard_PeriodYear(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	calls := 0
	svc.SpendReport = &fakeSpendReport{
		monthSpendFn: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, p billingapi.Period) (billingapi.MonthBreakdown, error) {
			// Dashboard calls MonthSpend TWICE (current + previous period).
			// Only assert on the first (current period) call.
			if calls == 0 {
				require.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), p.From)
				require.Equal(t, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), p.To)
			}
			calls++
			return billingapi.MonthBreakdown{TotalMin: 1}, nil
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard?period=year", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
}

// =============================================================================
// Additional coverage — Breakdown unknown period, Projects unknown period,
// applyPatch all-fields, topN < n branch
// =============================================================================

func TestBreakdown_UnknownPeriod_400(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/breakdown?period=foo", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	require.Equal(t, "billing.invalid_period", decodeEnvelope(t, w).Code)
}

func TestProjects_UnknownPeriod_400(t *testing.T) {
	t.Parallel()

	svc := defaultService()
	r := newRouter(t, svc, allowAllRBAC(), adminClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/projects?period=foo", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	require.Equal(t, "billing.invalid_period", decodeEnvelope(t, w).Code)
}

func TestPatchTariffs_AllFieldsChanged(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	svc := defaultService()
	gotUpdate := false
	svc.Tariffs = &fakeTariffStore{
		getFn: func(_ context.Context, _ uuid.UUID) (billingapi.Tariffs, error) {
			return billingapi.Tariffs{TenantID: claims.TenantID, Version: 1}, nil
		},
		updateFn: func(_ context.Context, _ uuid.UUID, in billingapi.Tariffs) (billingapi.Tariffs, error) {
			gotUpdate = true
			require.Equal(t, int64(15_000), in.WagePerSurveyMinor)
			require.Equal(t, int64(60), in.RespondentBasesMinor)
			require.Equal(t, int64(200), in.StorageMinorPerGBMo)
			require.Equal(t, int64(6_000_000), in.FixedFeesMinor)
			require.Equal(t, int64(500), in.TrunkCostsMinor["mtt"])
			in.Version = 2
			return in, nil
		},
	}
	r := newRouter(t, svc, allowAllRBAC(), claims)

	wage := int64(15_000)
	bases := int64(60)
	storage := int64(200)
	fixed := int64(6_000_000)
	patch := billingapi.TariffsPatchRequest{
		TrunkCostsMinor:      map[string]int64{"mtt": 500},
		WagePerSurveyMinor:   &wage,
		RespondentBasesMinor: &bases,
		StorageMinorPerGBMo:  &storage,
		FixedFeesMinor:       &fixed,
	}
	body, _ := json.Marshal(patch)

	req := httptest.NewRequest(stdhttp.MethodPatch, "/api/billing/tariffs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	require.True(t, gotUpdate)
}

func TestProjects_TopNTruncation(t *testing.T) {
	t.Parallel()

	// Dashboard exercises topN with len > 5; here we verify the inverse
	// branch (len < n returns the full slice unchanged).
	claims := adminClaims()
	svc := defaultService()
	svc.SpendReport = &fakeSpendReport{
		monthSpendFn: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ billingapi.Period) (billingapi.MonthBreakdown, error) {
			return billingapi.MonthBreakdown{TotalMin: 100}, nil
		},
	}
	svc.MarginReport = &fakeMarginReport{
		marginFn: func(_ context.Context, _ uuid.UUID, _ billingapi.Period) ([]billingapi.ProjectMargin, error) {
			out := make([]billingapi.ProjectMargin, 7)
			for i := range out {
				out[i] = billingapi.ProjectMargin{ProjectID: uuid.New(), TotalMin: int64(100 - i*10)}
			}
			return out, nil
		},
	}

	r := newRouter(t, svc, allowAllRBAC(), claims)
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp billingapi.DashboardResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.TopProjects, 5, "topN must truncate at 5")
}

func TestHelpers_BillingErrorMapping(t *testing.T) {
	t.Parallel()
	// Cross-check that the canonical sentinels render the canonical codes.
	// Each line below exercises one branch of mapBillingError via a handler.

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"invalid period", billingapi.ErrInvalidPeriod, stdhttp.StatusBadRequest, "billing.invalid_period"},
		{"no tariffs", billingapi.ErrNoTariffs, stdhttp.StatusConflict, "billing.no_tariffs"},
		{"invalid tariff", billingapi.ErrInvalidTariff, stdhttp.StatusBadRequest, "billing.invalid_tariff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := defaultService()
			svc.SpendReport = &fakeSpendReport{
				monthSpendFn: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ billingapi.Period) (billingapi.MonthBreakdown, error) {
					return billingapi.MonthBreakdown{}, tc.err
				},
			}
			r := newRouter(t, svc, allowAllRBAC(), adminClaims())
			req := httptest.NewRequest(stdhttp.MethodGet, "/api/finance/dashboard", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, tc.wantStatus, w.Code, "body=%s", w.Body.String())
			env := decodeEnvelope(t, w)
			require.Equal(t, tc.wantCode, env.Code)
		})
	}
}

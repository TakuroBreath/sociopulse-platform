package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	transporthttp "github.com/sociopulse/platform/internal/reports/transport/http"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// =============================================================================
// Fakes
// =============================================================================

// fakeRunner implements reportsapi.ReportRunner with an injectable runFn.
type fakeRunner struct {
	runFn func(context.Context, reportsapi.RunInput) (reportsapi.RunResult, error)
}

func (f *fakeRunner) Run(ctx context.Context, in reportsapi.RunInput) (reportsapi.RunResult, error) {
	if f.runFn != nil {
		return f.runFn(ctx, in)
	}
	return reportsapi.RunResult{}, nil
}

// fakeQueue implements reportsapi.JobQueue with injectable behaviour.
type fakeQueue struct {
	enqueueFn func(context.Context, reportsapi.JobInput) (reportsapi.JobTicket, error)
	getFn     func(context.Context, string) (reportsapi.Job, error)
	listFn    func(context.Context, reportsapi.ListJobsFilter) ([]reportsapi.Job, string, error)
	cancelFn  func(context.Context, string) error
}

func (f *fakeQueue) Enqueue(ctx context.Context, in reportsapi.JobInput) (reportsapi.JobTicket, error) {
	if f.enqueueFn != nil {
		return f.enqueueFn(ctx, in)
	}
	return reportsapi.JobTicket{}, nil
}

func (f *fakeQueue) Get(ctx context.Context, jobID string) (reportsapi.Job, error) {
	if f.getFn != nil {
		return f.getFn(ctx, jobID)
	}
	return reportsapi.Job{}, nil
}

func (f *fakeQueue) List(ctx context.Context, filter reportsapi.ListJobsFilter) ([]reportsapi.Job, string, error) {
	if f.listFn != nil {
		return f.listFn(ctx, filter)
	}
	return nil, "", nil
}

func (f *fakeQueue) Cancel(ctx context.Context, jobID string) error {
	if f.cancelFn != nil {
		return f.cancelFn(ctx, jobID)
	}
	return nil
}

// fakeResolver implements the transport's TenantResolver interface with
// an injectable lookup function.
type fakeResolver struct {
	fn func(context.Context, string) (uuid.UUID, error)
}

func (f *fakeResolver) SelectTenantByJobID(ctx context.Context, jobID string) (uuid.UUID, error) {
	if f.fn != nil {
		return f.fn(ctx, jobID)
	}
	return uuid.Nil, reportsapi.ErrJobNotFound
}

// injectClaims is the test analogue of pkg/middleware/auth.JWTMiddleware.
// Mirrors internal/recording/transport/http handlers_test.go pattern.
func injectClaims(claims authapi.Claims) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(authmw.ClaimsContextKey, claims)
		c.Next()
	}
}

// allowAdmin is a no-op RequireAdmin middleware that lets the request
// through. Tests that need to verify the admin-gate's behaviour swap
// this for a denying variant.
func allowAdmin() gin.HandlerFunc {
	return func(c *gin.Context) { c.Next() }
}

// adminClaims returns Claims with a fresh tenant and the admin role.
func adminClaims() authapi.Claims {
	return authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
}

// newRouter wires gin in TestMode, pre-attaches claims, then mounts the
// reports transport via transporthttp.Register.
func newRouter(handlers *transporthttp.Handlers, resolver transporthttp.TenantResolver, claims authapi.Claims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(injectClaims(claims))
	transporthttp.Register(r, transporthttp.RouterDeps{
		Handlers:     handlers,
		Resolver:     resolver,
		RequireAdmin: allowAdmin(),
	})
	return r
}

// =============================================================================
// Test fixtures
// =============================================================================

func validWindow() (time.Time, time.Time) {
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	return from, to
}

func validExportBody(kind reportsapi.ReportKind) []byte {
	from, to := validWindow()
	body := map[string]any{
		"format":      string(reportsapi.FormatXLSX),
		"params":      map[string]any{"project_id": uuid.NewString()},
		"window_from": from.Format(time.RFC3339Nano),
		"window_to":   to.Format(time.RFC3339Nano),
	}
	_ = kind
	b, _ := json.Marshal(body)
	return b
}

// =============================================================================
// ListKinds
// =============================================================================

func TestListKinds(t *testing.T) {
	t.Parallel()

	h := transporthttp.NewHandlersFromParts(&fakeRunner{}, &fakeQueue{})
	r := newRouter(h, &fakeResolver{}, adminClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/reports", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp struct {
		Kinds []reportsapi.ReportKind `json:"kinds"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Kinds, 7, "expect 6 predefined + custom")

	want := map[reportsapi.ReportKind]struct{}{
		reportsapi.KindOperatorEfficiency: {},
		reportsapi.KindProjectSummary:     {},
		reportsapi.KindCallsByStatus:      {},
		reportsapi.KindFinance:            {},
		reportsapi.KindQualityControl:     {},
		reportsapi.KindHourlyActivity:     {},
		reportsapi.KindCustom:             {},
	}
	for _, k := range resp.Kinds {
		_, ok := want[k]
		require.True(t, ok, "unexpected kind: %s", k)
	}
}

// =============================================================================
// Export — sync happy path
// =============================================================================

func TestExport_SyncHappy(t *testing.T) {
	t.Parallel()

	payload := []byte("X\x00fake-xlsx-bytes")
	mime := "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

	runner := &fakeRunner{
		runFn: func(_ context.Context, in reportsapi.RunInput) (reportsapi.RunResult, error) {
			// Verify the handler populated the input correctly.
			if in.Kind != reportsapi.KindOperatorEfficiency {
				t.Errorf("kind = %v, want operator_efficiency", in.Kind)
			}
			if in.Format != reportsapi.FormatXLSX {
				t.Errorf("format = %v, want xlsx", in.Format)
			}
			return reportsapi.RenderResult{
				Bytes:    payload,
				Filename: "operator_efficiency_20260501.xlsx",
				MIME:     mime,
				SHA256:   "deadbeef",
			}, nil
		},
	}
	h := transporthttp.NewHandlersFromParts(runner, &fakeQueue{})
	r := newRouter(h, &fakeResolver{}, adminClaims())

	body := validExportBody(reportsapi.KindOperatorEfficiency)
	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/reports/operator_efficiency/export", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Equal(t, mime, w.Header().Get("Content-Type"))
	require.Equal(t, payload, w.Body.Bytes())
}

// =============================================================================
// Export — auto-route to async on ErrAsyncRequired
// =============================================================================

func TestExport_AsyncRouting(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		runFn: func(_ context.Context, _ reportsapi.RunInput) (reportsapi.RunResult, error) {
			return reportsapi.RunResult{}, reportsapi.ErrAsyncRequired
		},
	}
	ticket := reportsapi.JobTicket{
		JobID:    "asynq-task-xyz-123",
		QueuedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
	}
	var enqueuedInput reportsapi.JobInput
	queue := &fakeQueue{
		enqueueFn: func(_ context.Context, in reportsapi.JobInput) (reportsapi.JobTicket, error) {
			enqueuedInput = in
			return ticket, nil
		},
	}
	h := transporthttp.NewHandlersFromParts(runner, queue)
	claims := adminClaims()
	r := newRouter(h, &fakeResolver{}, claims)

	body := validExportBody(reportsapi.KindOperatorEfficiency)
	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/reports/operator_efficiency/export", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusAccepted, w.Code, "body=%s", w.Body.String())
	require.Equal(t, claims.UserID, enqueuedInput.NotifyUserID,
		"NotifyUserID must equal the caller's user id")
	require.Equal(t, claims.TenantID, enqueuedInput.TenantID,
		"TenantID must equal the caller's claims tenant")

	var got reportsapi.JobTicket
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, ticket.JobID, got.JobID)
}

// =============================================================================
// Export — error mapping cases
// =============================================================================

func TestExport_RejectsUnknownKind(t *testing.T) {
	t.Parallel()

	h := transporthttp.NewHandlersFromParts(&fakeRunner{}, &fakeQueue{})
	r := newRouter(h, &fakeResolver{}, adminClaims())

	body := validExportBody("not_a_real_kind")
	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/reports/not_a_real_kind/export", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "reports.unknown_kind", env.Code)
}

func TestExport_RejectsInvalidWindow(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		runFn: func(_ context.Context, _ reportsapi.RunInput) (reportsapi.RunResult, error) {
			return reportsapi.RunResult{}, analyticsapi.ErrInvalidWindow
		},
	}
	h := transporthttp.NewHandlersFromParts(runner, &fakeQueue{})
	r := newRouter(h, &fakeResolver{}, adminClaims())

	body := validExportBody(reportsapi.KindOperatorEfficiency)
	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/reports/operator_efficiency/export", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "reports.window_invalid", env.Code)
}

func TestExport_RejectsErrTooLarge(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		runFn: func(_ context.Context, _ reportsapi.RunInput) (reportsapi.RunResult, error) {
			return reportsapi.RunResult{}, reportsapi.ErrTooLarge
		},
	}
	h := transporthttp.NewHandlersFromParts(runner, &fakeQueue{})
	r := newRouter(h, &fakeResolver{}, adminClaims())

	body := validExportBody(reportsapi.KindOperatorEfficiency)
	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/reports/operator_efficiency/export", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusUnprocessableEntity, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "reports.too_large", env.Code)
}

func TestExport_RejectsUnsupportedFormat(t *testing.T) {
	t.Parallel()

	h := transporthttp.NewHandlersFromParts(&fakeRunner{}, &fakeQueue{})
	r := newRouter(h, &fakeResolver{}, adminClaims())

	from, to := validWindow()
	body, _ := json.Marshal(map[string]any{
		"format":      "html",
		"window_from": from.Format(time.RFC3339Nano),
		"window_to":   to.Format(time.RFC3339Nano),
	})
	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/reports/operator_efficiency/export", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "reports.unsupported_format", env.Code)
}

// =============================================================================
// Custom — always async
// =============================================================================

func TestCustom_Always202(t *testing.T) {
	t.Parallel()

	ticket := reportsapi.JobTicket{
		JobID:    "asynq-task-custom-abc",
		QueuedAt: time.Date(2026, 5, 14, 12, 30, 0, 0, time.UTC),
	}
	queue := &fakeQueue{
		enqueueFn: func(_ context.Context, in reportsapi.JobInput) (reportsapi.JobTicket, error) {
			if in.Kind != reportsapi.KindCustom {
				t.Errorf("kind = %v, want custom", in.Kind)
			}
			return ticket, nil
		},
	}
	h := transporthttp.NewHandlersFromParts(&fakeRunner{}, queue)
	r := newRouter(h, &fakeResolver{}, adminClaims())

	body := validExportBody(reportsapi.KindCustom)
	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/reports/custom", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusAccepted, w.Code, "body=%s", w.Body.String())

	var got reportsapi.JobTicket
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, ticket.JobID, got.JobID)
}

// =============================================================================
// GetJob
// =============================================================================

func TestGetJob_Happy(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	jobID := "asynq-job-123"
	job := reportsapi.Job{
		ID:          jobID,
		TenantID:    claims.TenantID,
		Kind:        reportsapi.KindOperatorEfficiency,
		Format:      reportsapi.FormatXLSX,
		State:       reportsapi.JobSucceeded,
		BytesSize:   12345,
		Filename:    "operator_efficiency.xlsx",
		DownloadURL: "http://localobjectstore/sociopulse-reports-foo/operator_efficiency.xlsx?stub=true&expires=123",
		CreatedBy:   claims.UserID,
		CreatedAt:   time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
	}
	queue := &fakeQueue{
		getFn: func(_ context.Context, id string) (reportsapi.Job, error) {
			require.Equal(t, jobID, id)
			return job, nil
		},
	}
	resolver := &fakeResolver{
		fn: func(_ context.Context, id string) (uuid.UUID, error) {
			require.Equal(t, jobID, id)
			return claims.TenantID, nil
		},
	}
	h := transporthttp.NewHandlersFromParts(&fakeRunner{}, queue)
	r := newRouter(h, resolver, claims)

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/reports/jobs/"+jobID, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())

	var got reportsapi.Job
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, jobID, got.ID)
	require.Equal(t, reportsapi.JobSucceeded, got.State)
}

func TestGetJob_NotFound(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	jobID := "asynq-job-missing"
	resolver := &fakeResolver{
		fn: func(_ context.Context, _ string) (uuid.UUID, error) {
			return uuid.Nil, reportsapi.ErrJobNotFound
		},
	}
	h := transporthttp.NewHandlersFromParts(&fakeRunner{}, &fakeQueue{})
	r := newRouter(h, resolver, claims)

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/reports/jobs/"+jobID, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusNotFound, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "reports.job_not_found", env.Code)
}

// =============================================================================
// Download
// =============================================================================

func TestDownload_RedirectsToPresignedURL(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	jobID := "asynq-job-success"
	presigned := "http://localobjectstore/sociopulse-reports-foo/result.xlsx?stub=true&expires=999"

	resolver := &fakeResolver{
		fn: func(_ context.Context, _ string) (uuid.UUID, error) {
			return claims.TenantID, nil
		},
	}
	queue := &fakeQueue{
		getFn: func(_ context.Context, _ string) (reportsapi.Job, error) {
			return reportsapi.Job{
				ID:          jobID,
				TenantID:    claims.TenantID,
				State:       reportsapi.JobSucceeded,
				DownloadURL: presigned,
			}, nil
		},
	}
	h := transporthttp.NewHandlersFromParts(&fakeRunner{}, queue)
	r := newRouter(h, resolver, claims)

	req := httptest.NewRequest(stdhttp.MethodGet,
		"/api/reports/jobs/"+jobID+"/download", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusFound, w.Code, "body=%s", w.Body.String())
	require.Equal(t, presigned, w.Header().Get("Location"))
}

func TestDownload_RejectsRunningJob(t *testing.T) {
	t.Parallel()

	claims := adminClaims()
	jobID := "asynq-job-running"

	resolver := &fakeResolver{
		fn: func(_ context.Context, _ string) (uuid.UUID, error) {
			return claims.TenantID, nil
		},
	}
	queue := &fakeQueue{
		getFn: func(_ context.Context, _ string) (reportsapi.Job, error) {
			return reportsapi.Job{
				ID:       jobID,
				TenantID: claims.TenantID,
				State:    reportsapi.JobRunning,
			}, nil
		},
	}
	h := transporthttp.NewHandlersFromParts(&fakeRunner{}, queue)
	r := newRouter(h, resolver, claims)

	req := httptest.NewRequest(stdhttp.MethodGet,
		"/api/reports/jobs/"+jobID+"/download", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusConflict, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "reports.job_not_ready", env.Code)
}

// =============================================================================
// jobIDTenantGuard — cross-tenant probe defence
// =============================================================================

func TestJobIDTenantGuard_CrossTenant_Returns404(t *testing.T) {
	t.Parallel()

	callerClaims := adminClaims() // Tenant A
	otherTenant := uuid.New()     // Tenant B
	require.NotEqual(t, callerClaims.TenantID, otherTenant)

	resolver := &fakeResolver{
		fn: func(_ context.Context, _ string) (uuid.UUID, error) {
			return otherTenant, nil // job belongs to Tenant B
		},
	}
	queueCalled := false
	queue := &fakeQueue{
		getFn: func(_ context.Context, _ string) (reportsapi.Job, error) {
			queueCalled = true
			return reportsapi.Job{}, nil
		},
	}
	h := transporthttp.NewHandlersFromParts(&fakeRunner{}, queue)
	r := newRouter(h, resolver, callerClaims)

	req := httptest.NewRequest(stdhttp.MethodGet,
		"/api/reports/jobs/some-job-id", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusNotFound, w.Code,
		"cross-tenant lookup MUST 404 (existence-probe defence), never 403")
	require.False(t, queueCalled, "guard must abort before handler runs")

	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "reports.job_not_found", env.Code)
}

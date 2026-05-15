package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	rapi "github.com/sociopulse/platform/internal/recording/api"
	transporthttp "github.com/sociopulse/platform/internal/recording/transport/http"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// =============================================================================
// Fakes
// =============================================================================

// fakeRecordingService implements rapi.RecordingService with optional
// override functions. Methods that aren't under test return zero values
// — Commit and Get are unused by the HTTP transport but must be present
// to satisfy the interface.
type fakeRecordingService struct {
	streamFn func(context.Context, uuid.UUID, uuid.UUID, *rapi.ByteRange) (rapi.AudioStream, error)
	searchFn func(context.Context, uuid.UUID, rapi.SearchQuery) (rapi.SearchResult, error)
	verifyFn func(context.Context, uuid.UUID, uuid.UUID) (rapi.VerifyResult, error)
}

func (f *fakeRecordingService) Commit(_ context.Context, _ rapi.CommitInput) (rapi.CommitOutput, error) {
	return rapi.CommitOutput{}, nil
}

func (f *fakeRecordingService) Get(_ context.Context, _, _ uuid.UUID) (rapi.RecordingMetadata, error) {
	return rapi.RecordingMetadata{}, nil
}

func (f *fakeRecordingService) Search(ctx context.Context, tenantID uuid.UUID, q rapi.SearchQuery) (rapi.SearchResult, error) {
	if f.searchFn != nil {
		return f.searchFn(ctx, tenantID, q)
	}
	return rapi.SearchResult{}, nil
}

func (f *fakeRecordingService) OpenAudioStream(ctx context.Context, tenantID, callID uuid.UUID, br *rapi.ByteRange) (rapi.AudioStream, error) {
	if f.streamFn != nil {
		return f.streamFn(ctx, tenantID, callID, br)
	}
	return rapi.AudioStream{}, nil
}

func (f *fakeRecordingService) VerifyChecksum(ctx context.Context, tenantID, callID uuid.UUID) (rapi.VerifyResult, error) {
	if f.verifyFn != nil {
		return f.verifyFn(ctx, tenantID, callID)
	}
	return rapi.VerifyResult{}, nil
}

// injectClaims is the test analogue of pkg/middleware/auth.JWTMiddleware.
// It puts the supplied Claims onto the gin.Context under the same key
// (authmw.ClaimsContextKey) that claimsFromContext reads from, so the
// transport-under-test can be exercised without a real JWT validator.
//
// This pattern mirrors internal/dialer/transport/http/middleware_test.go's
// injectClaims helper — the canonical project pattern.
func injectClaims(claims authapi.Claims) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(authmw.ClaimsContextKey, claims)
		c.Next()
	}
}

// newRouterWithClaims wires gin in TestMode and pre-attaches the supplied
// claims via injectClaims, then mounts the recording transport. Tests
// pass nil Validator so JWTMiddleware doesn't run — the test claims
// already live on the context.
func newRouterWithClaims(svc rapi.RecordingService, claims authapi.Claims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(injectClaims(claims))
	transporthttp.Mount(api, transporthttp.Deps{Service: svc})
	return r
}

// adminClaims / supervisorClaims / operatorClaims yield Claims with a
// randomly-allocated TenantID and the named role.
func adminClaims() authapi.Claims {
	return authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
}

func supervisorClaims() authapi.Claims {
	return authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleSupervisor},
	}
}

func operatorClaims() authapi.Claims {
	return authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleOperator},
	}
}

// =============================================================================
// streamRecording — GET /api/calls/:id/recording
// =============================================================================

func TestStreamRecording_OK(t *testing.T) {
	t.Parallel()

	payload := []byte("hello world audio bytes")
	svc := &fakeRecordingService{
		streamFn: func(_ context.Context, _, _ uuid.UUID, _ *rapi.ByteRange) (rapi.AudioStream, error) {
			return rapi.AudioStream{
				Reader:        io.NopCloser(bytes.NewReader(payload)),
				ContentType:   "audio/ogg",
				ContentLength: int64(len(payload)),
			}, nil
		},
	}
	r := newRouterWithClaims(svc, supervisorClaims())

	callID := uuid.New()
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/calls/"+callID.String()+"/recording", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "audio/ogg", w.Header().Get("Content-Type"))
	require.Equal(t, "23", w.Header().Get("Content-Length"))
	require.Equal(t, "none", w.Header().Get("Accept-Ranges"))
	require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	require.Equal(t, payload, w.Body.Bytes())
}

func TestStreamRecording_NotFound(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{
		streamFn: func(_ context.Context, _, _ uuid.UUID, _ *rapi.ByteRange) (rapi.AudioStream, error) {
			return rapi.AudioStream{}, rapi.ErrNotFound
		},
	}
	r := newRouterWithClaims(svc, adminClaims())

	callID := uuid.New()
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/calls/"+callID.String()+"/recording", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusNotFound, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "recording.not_found", env.Code)
}

func TestStreamRecording_BadCallID(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{}
	r := newRouterWithClaims(svc, supervisorClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/calls/not-a-uuid/recording", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "recording.invalid_input", env.Code)
}

func TestStreamRecording_RBACForbidden(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{}
	r := newRouterWithClaims(svc, operatorClaims())

	callID := uuid.New()
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/calls/"+callID.String()+"/recording", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusForbidden, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "auth.insufficient_role", env.Code)
}

// =============================================================================
// searchRecordings — GET /api/recordings/search
// =============================================================================

func TestSearch_OK(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{
		searchFn: func(_ context.Context, _ uuid.UUID, _ rapi.SearchQuery) (rapi.SearchResult, error) {
			return rapi.SearchResult{Items: nil, NextCursor: "", HasMore: false}, nil
		},
	}
	r := newRouterWithClaims(svc, supervisorClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/recordings/search", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp transporthttp.SearchResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.False(t, resp.HasMore)
	require.Empty(t, resp.NextCursor)
	require.Empty(t, resp.Items)
}

func TestSearch_OK_PassesQueryFiltersThrough(t *testing.T) {
	t.Parallel()

	projectID := uuid.New()
	operatorID := uuid.New()
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	var got rapi.SearchQuery
	svc := &fakeRecordingService{
		searchFn: func(_ context.Context, _ uuid.UUID, q rapi.SearchQuery) (rapi.SearchResult, error) {
			got = q
			rid := uuid.New()
			cid := uuid.New()
			tid := uuid.New()
			return rapi.SearchResult{
				Items: []rapi.RecordingMetadata{{
					RecordingID: rid,
					CallID:      cid,
					TenantID:    tid,
					BytesSize:   12345,
					Duration:    1500 * time.Millisecond,
					SHA256Hex:   "deadbeef",
					Status:      "stored",
					CommittedAt: time.Now().UTC(),
				}},
				NextCursor: "cursor-foo",
				HasMore:    true,
			}, nil
		},
	}
	r := newRouterWithClaims(svc, adminClaims())

	url := "/api/recordings/search?project_id=" + projectID.String() +
		"&operator_id=" + operatorID.String() +
		"&status=stored,cold" +
		"&from=" + from.Format(time.RFC3339) +
		"&limit=25"
	req := httptest.NewRequest(stdhttp.MethodGet, url, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, got.ProjectID)
	require.Equal(t, projectID, *got.ProjectID)
	require.NotNil(t, got.OperatorID)
	require.Equal(t, operatorID, *got.OperatorID)
	require.Equal(t, []string{"stored", "cold"}, got.Status)
	require.NotNil(t, got.From)
	require.True(t, got.From.Equal(from))
	require.Equal(t, 25, got.Limit)

	var resp transporthttp.SearchResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.HasMore)
	require.Equal(t, "cursor-foo", resp.NextCursor)
	require.Len(t, resp.Items, 1)
	require.Equal(t, int64(1500), resp.Items[0].DurationMS)
}

func TestSearch_BadLimitReturns400(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{}
	r := newRouterWithClaims(svc, supervisorClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/recordings/search?limit=abc", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "recording.invalid_input", env.Code)
}

func TestSearch_BadFromReturns400(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{}
	r := newRouterWithClaims(svc, supervisorClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/recordings/search?from=not-a-time", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusBadRequest, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "recording.invalid_input", env.Code)
}

func TestSearch_RBACForbidden(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{}
	r := newRouterWithClaims(svc, operatorClaims())

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/recordings/search", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusForbidden, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "auth.insufficient_role", env.Code)
}

// =============================================================================
// verifyChecksum — POST /api/calls/:id/recording/verify
// =============================================================================

func TestVerify_OK(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{
		verifyFn: func(_ context.Context, _, _ uuid.UUID) (rapi.VerifyResult, error) {
			return rapi.VerifyResult{
				OK:           true,
				ExpectedSHA:  "abc",
				ActualSHA:    "abc",
				BytesScanned: 100,
				DurationMS:   42,
			}, nil
		},
	}
	r := newRouterWithClaims(svc, adminClaims())

	callID := uuid.New()
	req := httptest.NewRequest(stdhttp.MethodPost, "/api/calls/"+callID.String()+"/recording/verify", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp transporthttp.VerifyResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.OK)
	require.Equal(t, "abc", resp.ExpectedSHA)
	require.Equal(t, int64(100), resp.BytesScanned)
}

func TestVerify_RBACSupervisorForbidden(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{}
	r := newRouterWithClaims(svc, supervisorClaims())

	callID := uuid.New()
	req := httptest.NewRequest(stdhttp.MethodPost, "/api/calls/"+callID.String()+"/recording/verify", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// supervisor is allowed to read & search but verify is admin-only.
	require.Equal(t, stdhttp.StatusForbidden, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "auth.insufficient_role", env.Code)
}

func TestVerify_AlreadyDeleted(t *testing.T) {
	t.Parallel()

	svc := &fakeRecordingService{
		verifyFn: func(_ context.Context, _, _ uuid.UUID) (rapi.VerifyResult, error) {
			return rapi.VerifyResult{}, rapi.ErrAlreadyDeleted
		},
	}
	r := newRouterWithClaims(svc, adminClaims())

	callID := uuid.New()
	req := httptest.NewRequest(stdhttp.MethodPost, "/api/calls/"+callID.String()+"/recording/verify", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusGone, w.Code)
	var env transporthttp.ErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "recording.already_deleted", env.Code)
}

// =============================================================================
// Mount edge cases
// =============================================================================

func TestRoutes_NoServiceDoesNotMount(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	// Mount with a nil Service — the transport must NOT register routes.
	// Subsequent requests must hit gin's default 404.
	r := gin.New()
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{Service: nil})

	callID := uuid.New()
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/calls/"+callID.String()+"/recording", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusNotFound, w.Code,
		"Mount must be a no-op when Service is nil so the route is unregistered")
}

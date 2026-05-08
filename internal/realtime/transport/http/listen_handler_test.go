package http

import (
	"bytes"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// listenFixture wires a minimal listen-handler stack: gin engine +
// JWTMiddleware (so claims flow through) + ListenHandler routes.
type listenFixture struct {
	router    *gin.Engine
	validator *fakeClaimsValidator
}

func newListenFixture(t *testing.T) *listenFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	validator := &fakeClaimsValidator{
		claims: authapi.Claims{
			UserID:   uuid.New(),
			TenantID: uuid.New(),
			Roles:    []authapi.Role{authapi.RoleSupervisor},
		},
	}
	r := gin.New()
	api := r.Group("/api/realtime")
	api.Use(authmw.JWTMiddleware(validator))
	h := newListenHandler(zap.NewNop())
	h.mount(api)
	return &listenFixture{router: r, validator: validator}
}

func (fx *listenFixture) do(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBuffer(nil))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	fx.router.ServeHTTP(rr, req)
	return rr
}

// TestListenHandler_StartReturns503 verifies POST /calls/:id/listen
// returns a Service Unavailable + the canonical telephony.bridge.offline
// envelope per Plan 11 Decision 5 (listen-in deferred until Plan 08).
func TestListenHandler_StartReturns503(t *testing.T) {
	t.Parallel()
	fx := newListenFixture(t)

	rr := fx.do(t, stdhttp.MethodPost, "/api/realtime/calls/"+uuid.NewString()+"/listen")
	assert.Equal(t, stdhttp.StatusServiceUnavailable, rr.Code)

	var body errorEnvelope
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "telephony.bridge.offline", body.Code)
	assert.Contains(t, body.Message, "Plan 08")
}

// TestListenHandler_StopReturns503 verifies DELETE
// /listen-sessions/:id returns 503 — the symmetric stub for the stop
// path.
func TestListenHandler_StopReturns503(t *testing.T) {
	t.Parallel()
	fx := newListenFixture(t)

	rr := fx.do(t, stdhttp.MethodDelete, "/api/realtime/listen-sessions/"+uuid.NewString())
	assert.Equal(t, stdhttp.StatusServiceUnavailable, rr.Code)

	var body errorEnvelope
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "telephony.bridge.offline", body.Code)
}

// TestListenHandler_NoAuthReturns401 verifies the JWTMiddleware gate
// fires for an unauthenticated request — the listen-in stubs run
// behind the same auth chain as the rest of the realtime transport.
func TestListenHandler_NoAuthReturns401(t *testing.T) {
	t.Parallel()
	fx := newListenFixture(t)

	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/realtime/calls/"+uuid.NewString()+"/listen",
		bytes.NewBuffer(nil))
	rr := httptest.NewRecorder()
	fx.router.ServeHTTP(rr, req)
	assert.Equal(t, stdhttp.StatusUnauthorized, rr.Code)
}

// TestListenHandler_TokenInvalidReturns401 verifies an
// authentication failure surfaces as 401 (the listen-in stub is
// behind the standard gate).
func TestListenHandler_TokenInvalidReturns401(t *testing.T) {
	t.Parallel()
	fx := newListenFixture(t)
	fx.validator.err = authapi.ErrTokenInvalid

	rr := fx.do(t, stdhttp.MethodPost, "/api/realtime/calls/"+uuid.NewString()+"/listen")
	assert.Equal(t, stdhttp.StatusUnauthorized, rr.Code)
}

// TestListenHandler_NopLoggerSafe verifies the constructor accepts a
// nil logger and the handler still works (defensive nil-check).
func TestListenHandler_NopLoggerSafe(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	validator := &fakeClaimsValidator{
		claims: authapi.Claims{
			UserID:   uuid.New(),
			TenantID: uuid.New(),
			Roles:    []authapi.Role{authapi.RoleSupervisor},
		},
	}
	r := gin.New()
	api := r.Group("/api/realtime")
	api.Use(authmw.JWTMiddleware(validator))
	h := newListenHandler(nil) // nil logger
	h.mount(api)

	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/realtime/calls/"+uuid.NewString()+"/listen",
		bytes.NewBuffer(nil))
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, stdhttp.StatusServiceUnavailable, rr.Code)
}

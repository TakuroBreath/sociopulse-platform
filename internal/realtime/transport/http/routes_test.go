package http

import (
	"bytes"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// TestMount_RegistersAllRoutes verifies the canonical Mount call wires
// every endpoint the package exports. We hit each route with a fake
// JWT validator and assert the expected status — exact bodies are
// covered by the per-handler tests.
func TestMount_RegistersAllRoutes(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)
	connMetrics := service.RegisterMetrics(reg)

	tenantID := uuid.New()
	userID := uuid.New()

	validator := &fakeClaimsValidator{
		claims: authapi.Claims{
			UserID:   userID,
			TenantID: tenantID,
			Roles:    []authapi.Role{authapi.RoleAdmin},
		},
	}
	rtClaimsAuth := &stubAuthValidator{
		token:  "tok",
		claims: rtapiClaimsFor(tenantID.String(), userID.String(), []string{"admin"}),
	}

	r := gin.New()
	api := r.Group("/api/realtime")

	Mount(api, Deps{
		Hub:             hub,
		AuthValidator:   rtClaimsAuth,
		ClaimsValidator: validator,
		ConnMetrics:     connMetrics,
		Logger:          logger,
	})

	// Force-pause -> 202 (admin role).
	rr := doAuthed(r, stdhttp.MethodPost,
		"/api/realtime/operators/"+uuid.NewString()+"/force-pause", "tok")
	assert.Equal(t, stdhttp.StatusAccepted, rr.Code, "force-pause: %s", rr.Body.String())

	// Listen-in start -> 503 (Plan 08 deferred).
	rr = doAuthed(r, stdhttp.MethodPost,
		"/api/realtime/calls/"+uuid.NewString()+"/listen", "tok")
	assert.Equal(t, stdhttp.StatusServiceUnavailable, rr.Code, "listen: %s", rr.Body.String())

	// Listen-in stop -> 503.
	rr = doAuthed(r, stdhttp.MethodDelete,
		"/api/realtime/listen-sessions/"+uuid.NewString(), "tok")
	assert.Equal(t, stdhttp.StatusServiceUnavailable, rr.Code, "stop: %s", rr.Body.String())

	// /ws is the only route NOT routed through JWTMiddleware (the
	// realtime AuthHandshake reads the token from the FrameAuth on
	// the wire). We assert it's mounted by hitting it without
	// upgrading — gin returns the upgrade-required response from
	// websocket.Accept.
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/realtime/ws", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	// websocket.Accept rejects non-upgrade requests with 400/426.
	assert.NotEqual(t, stdhttp.StatusNotFound, rr.Code,
		"/ws should be mounted; got 404 — Mount missed the WS handler")
}

// rtapiClaimsFor builds an rtapi.Claims with the listed roles. Tiny
// helper to keep the test clean — production code uses authAdapter.
func rtapiClaimsFor(tenantID, userID string, roles []string) (claims rtapiClaimsAlias) {
	claims = rtapiClaimsAlias{TenantID: tenantID, UserID: userID, Roles: roles}
	return claims
}

// rtapiClaimsAlias is a local alias to avoid an explicit rtapi import
// in the routes test file's helper.
type rtapiClaimsAlias = struct {
	UserID   string
	TenantID string
	Roles    []string
}

// doAuthed sends an HTTP request with a Bearer token attached so the
// JWTMiddleware accepts it.
func doAuthed(r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBuffer(nil))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// TestMount_NilHub_Panics verifies missing required Deps surface as a
// boot-time panic so the composition root in cmd/api fails loudly
// rather than at first request.
func TestMount_NilHub_Panics(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/realtime")
	require.Panics(t, func() {
		Mount(api, Deps{
			AuthValidator:   &stubAuthValidator{},
			ClaimsValidator: &fakeClaimsValidator{},
			Logger:          zap.NewNop(),
		})
	})
}

func TestMount_NilAuth_Panics(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)
	r := gin.New()
	api := r.Group("/api/realtime")
	require.Panics(t, func() {
		Mount(api, Deps{
			Hub:             hub,
			ClaimsValidator: &fakeClaimsValidator{},
			Logger:          logger,
		})
	})
}

func TestMount_NilClaimsValidator_Panics(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)
	r := gin.New()
	api := r.Group("/api/realtime")
	require.Panics(t, func() {
		Mount(api, Deps{
			Hub:           hub,
			AuthValidator: &stubAuthValidator{},
			Logger:        logger,
		})
	})
}

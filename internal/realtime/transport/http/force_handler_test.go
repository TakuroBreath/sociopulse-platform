package http

import (
	"bytes"
	"context"
	"encoding/json"
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
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// fakeClaimsValidator is the auth-side stub used by tests that mount
// the JWTMiddleware. Returns canned authapi.Claims for any token.
type fakeClaimsValidator struct {
	claims authapi.Claims
	err    error
}

func (f *fakeClaimsValidator) Validate(_ context.Context, _ string) (authapi.Claims, error) {
	if f.err != nil {
		return authapi.Claims{}, f.err
	}
	return f.claims, nil
}

// forceFixture wires a complete force-handler test stack: hub +
// connection + JWT middleware + ForceHandler routes. The test broadcasts
// to a real *Connection registered with the Hub via AttachForTest, then
// drains the conn's send queue to assert the operator-bound payload
// landed on its wire.
type forceFixture struct {
	router      *gin.Engine
	hub         *service.Hub
	conn        *service.Connection
	tenantID    uuid.UUID
	operatorOps func(*authapi.Claims)
}

func newForceFixture(t *testing.T) (*forceFixture, *fakeClaimsValidator) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)

	tenantID := uuid.New()
	operatorID := uuid.New()
	adminID := uuid.New()

	// Register a *Connection in the Hub for the operator. The
	// connection has no live WS — we just need it as a target for
	// Hub.Broadcast.
	connConfig := service.ConnectionConfig{
		AuthTimeout:     1,
		PingPeriod:      1,
		PongTimeout:     1,
		WriteTimeout:    1,
		WriteBufferSize: 16,
		Logger:          logger,
	}
	conn := service.NewConnection(stubWS{}, connConfig)
	hub.AttachForTest(conn, rtapi.Claims{
		UserID:   operatorID.String(),
		TenantID: tenantID.String(),
		Roles:    []string{"operator"},
	})
	_, err := conn.Subscribe(rtapi.TopicForceCommands, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	validator := &fakeClaimsValidator{
		claims: authapi.Claims{
			UserID:   adminID,
			TenantID: tenantID,
			Roles:    []authapi.Role{authapi.RoleAdmin},
		},
	}

	r := gin.New()
	api := r.Group("/api/realtime")
	api.Use(authmw.JWTMiddleware(validator))
	h := newForceHandler(forceHandlerConfig{
		hub:    hub,
		logger: logger,
	})
	h.mount(api)

	fx := &forceFixture{
		router:   r,
		hub:      hub,
		conn:     conn,
		tenantID: tenantID,
	}
	// expose so tests can mutate the validator's claims (e.g. drop the
	// admin role for the role-denied case).
	fx.operatorOps = func(_ *authapi.Claims) {}
	_ = operatorID
	return fx, validator
}

// stubWS is a minimal rtapi.WSConn that swallows Reads / Writes /
// Closes. Used as a Hub-attach target when the test only cares about
// the per-conn sendChan delivery (Drain-for-test).
type stubWS struct{}

func (stubWS) ReadFrame(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (stubWS) WriteFrame(_ context.Context, _ []byte) error { return nil }
func (stubWS) Close(_ rtapi.CloseReason, _ string) error    { return nil }
func (stubWS) RemoteAddr() string                           { return "127.0.0.1:0" }

func (fx *forceFixture) doPost(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(stdhttp.MethodPost, path, bytes.NewBuffer(nil))
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	fx.router.ServeHTTP(rr, req)
	return rr
}

// TestForceHandler_Pause_Admin_Returns202 verifies the happy path:
// admin POSTs to /operators/:id/force-pause; the handler broadcasts
// on TopicForceCommands and the operator's connection receives the
// frame.
func TestForceHandler_Pause_Admin_Returns202(t *testing.T) {
	t.Parallel()
	fx, _ := newForceFixture(t)
	operatorID := fx.conn.Claims().UserID

	rr := fx.doPost(t, "/api/realtime/operators/"+operatorID+"/force-pause")
	require.Equal(t, stdhttp.StatusAccepted, rr.Code, rr.Body.String())

	var body forceResponseDTO
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, 1, body.Recipients)

	// Drain the operator's send queue to verify the broadcast was
	// queued with TopicForceCommands.
	frame := fx.conn.DrainSendForTest()
	require.NotNil(t, frame)
	assert.Equal(t, rtapi.FrameEvent, frame.Type)
	assert.Equal(t, rtapi.TopicForceCommands, frame.Topic)
	// Payload should include the action.
	var payload forcePayloadDTO
	require.NoError(t, json.Unmarshal(frame.Payload, &payload))
	assert.Equal(t, forceActionPause, payload.Action)
}

// TestForceHandler_EndShift_Admin_Returns202 mirrors the pause path
// for the force-end-shift action.
func TestForceHandler_EndShift_Admin_Returns202(t *testing.T) {
	t.Parallel()
	fx, _ := newForceFixture(t)
	operatorID := fx.conn.Claims().UserID

	rr := fx.doPost(t, "/api/realtime/operators/"+operatorID+"/force-end-shift")
	require.Equal(t, stdhttp.StatusAccepted, rr.Code, rr.Body.String())

	frame := fx.conn.DrainSendForTest()
	require.NotNil(t, frame)
	var payload forcePayloadDTO
	require.NoError(t, json.Unmarshal(frame.Payload, &payload))
	assert.Equal(t, forceActionEndShift, payload.Action)
}

// TestForceHandler_RejectsOperatorRole verifies a caller with
// operator-only claims gets 403.
func TestForceHandler_RejectsOperatorRole(t *testing.T) {
	t.Parallel()
	fx, validator := newForceFixture(t)
	validator.claims.Roles = []authapi.Role{authapi.RoleOperator}

	rr := fx.doPost(t, "/api/realtime/operators/"+fx.conn.Claims().UserID+"/force-pause")
	assert.Equal(t, stdhttp.StatusForbidden, rr.Code)
}

// TestForceHandler_AllowsSupervisor verifies supervisor role is
// accepted alongside admin.
func TestForceHandler_AllowsSupervisor(t *testing.T) {
	t.Parallel()
	fx, validator := newForceFixture(t)
	validator.claims.Roles = []authapi.Role{authapi.RoleSupervisor}

	rr := fx.doPost(t, "/api/realtime/operators/"+fx.conn.Claims().UserID+"/force-pause")
	assert.Equal(t, stdhttp.StatusAccepted, rr.Code, rr.Body.String())
}

// TestForceHandler_InvalidUUID_Returns400 verifies a non-UUID
// :id surfaces as 400 rather than reaching the Hub.
func TestForceHandler_InvalidUUID_Returns400(t *testing.T) {
	t.Parallel()
	fx, _ := newForceFixture(t)

	rr := fx.doPost(t, "/api/realtime/operators/not-a-uuid/force-pause")
	assert.Equal(t, stdhttp.StatusBadRequest, rr.Code)
}

// TestForceHandler_NoMatchingOperator_StillReturns202 verifies a
// broadcast that reaches zero local recipients still returns 202 with
// recipients=0 (cross-replica fan-out via NATS may still pick it up).
func TestForceHandler_NoMatchingOperator_StillReturns202(t *testing.T) {
	t.Parallel()
	fx, _ := newForceFixture(t)
	otherOperator := uuid.NewString()

	rr := fx.doPost(t, "/api/realtime/operators/"+otherOperator+"/force-pause")
	assert.Equal(t, stdhttp.StatusAccepted, rr.Code)

	var body forceResponseDTO
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, 0, body.Recipients)
}

// TestNewForceHandler_NilHubPanics verifies the constructor enforces
// the non-nil hub contract so a misconfigured Mount fails at boot.
func TestNewForceHandler_NilHubPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		newForceHandler(forceHandlerConfig{logger: zap.NewNop()})
	})
}

// TestNewForceHandler_NilLoggerSafe verifies a nil logger falls back
// to nop without panicking.
func TestNewForceHandler_NilLoggerSafe(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)
	h := newForceHandler(forceHandlerConfig{hub: hub, logger: nil})
	require.NotNil(t, h)
}

// TestForceHandler_NoAuth_Returns401 verifies the JWTMiddleware gate
// fires for an unauthenticated request — defence-in-depth above the
// in-handler claims read.
func TestForceHandler_NoAuth_Returns401(t *testing.T) {
	t.Parallel()
	fx, _ := newForceFixture(t)

	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/realtime/operators/"+fx.conn.Claims().UserID+"/force-pause",
		bytes.NewBuffer(nil))
	rr := httptest.NewRecorder()
	fx.router.ServeHTTP(rr, req)
	assert.Equal(t, stdhttp.StatusUnauthorized, rr.Code)
}

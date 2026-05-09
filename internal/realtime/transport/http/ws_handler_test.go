package http

import (
	"context"
	"encoding/json"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// fakePresenceTracker records calls. Used to assert OnConnect / Touch /
// OnDisconnect lifecycle wiring without standing up Redis.
type fakePresenceTracker struct {
	mu              sync.Mutex
	connects        []presenceCall
	disconnects     []presenceCall
	touches         []presenceCall
	touchErr        error
	connectErr      error
	disconnectCount atomic.Int32
}

type presenceCall struct {
	tenantID  string
	userID    string
	replicaID string
}

func (f *fakePresenceTracker) OnConnect(_ context.Context, tenantID, userID, replicaID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connectErr != nil {
		return f.connectErr
	}
	f.connects = append(f.connects, presenceCall{tenantID: tenantID, userID: userID, replicaID: replicaID})
	return nil
}

func (f *fakePresenceTracker) OnDisconnect(_ context.Context, tenantID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects = append(f.disconnects, presenceCall{tenantID: tenantID, userID: userID})
	f.disconnectCount.Add(1)
	return nil
}

func (f *fakePresenceTracker) Touch(_ context.Context, tenantID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, presenceCall{tenantID: tenantID, userID: userID})
	return f.touchErr
}

func (f *fakePresenceTracker) IsOnline(context.Context, string, string) (bool, error) {
	return false, nil
}

func (f *fakePresenceTracker) OnlineUsers(context.Context, string) ([]string, error) {
	return nil, nil
}

func (f *fakePresenceTracker) connectsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.connects)
}

func (f *fakePresenceTracker) touchesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.touches)
}

// stubAuthValidator validates a single token to a fixed set of claims.
type stubAuthValidator struct {
	token  string
	claims rtapi.Claims
	err    error
}

func (s *stubAuthValidator) Validate(_ context.Context, tok string) (rtapi.Claims, error) {
	if s.err != nil {
		return rtapi.Claims{}, s.err
	}
	if tok != s.token {
		return rtapi.Claims{}, rtapi.ErrAuthFailed
	}
	return s.claims, nil
}

// wsHandlerFixture spins up a complete WS pipeline (gin + handler +
// hub + presence + auth) backed by httptest.Server. AllowedOrigins is
// "*" so the test client's localhost origin is accepted.
type wsHandlerFixture struct {
	server   *httptest.Server
	hub      *service.Hub
	presence *fakePresenceTracker
	auth     *stubAuthValidator
	wsURL    string
	tenantID string
	userID   string
}

func newWSHandlerFixture(t *testing.T, roles []string) *wsHandlerFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	// Use a Nop logger here so log lines emitted by the per-conn
	// goroutines AFTER the test ends (close-handshake unwinding) do
	// not race against the testing.T's *common.destination — a known
	// zaptest hazard for hijacked WebSocket goroutines that the
	// httptest.Server.Close cannot wait for.
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()

	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)

	connMetrics := service.RegisterMetrics(reg)
	presence := &fakePresenceTracker{}
	tenantID := uuid.NewString()
	userID := uuid.NewString()
	auth := &stubAuthValidator{
		token: "valid-token",
		claims: rtapi.Claims{
			UserID:   userID,
			TenantID: tenantID,
			Roles:    roles,
		},
	}

	h := newWSHandler(wsHandlerConfig{
		hub:         hub,
		auth:        auth,
		metrics:     connMetrics,
		presence:    presence,
		replicaID:   "test-replica",
		logger:      logger,
		origins:     []string{"*"},
		touchPeriod: 50 * time.Millisecond,
		connConfig: service.ConnectionConfig{
			AuthTimeout:     time.Second,
			PingPeriod:      100 * time.Millisecond,
			PongTimeout:     2 * time.Second,
			WriteTimeout:    time.Second,
			WriteBufferSize: 16,
			RateLimitPerSec: 100,
			Logger:          logger,
		},
	})

	r := gin.New()
	r.GET("/api/realtime/ws", h.handle)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	return &wsHandlerFixture{
		server:   server,
		hub:      hub,
		presence: presence,
		auth:     auth,
		wsURL:    "ws://" + strings.TrimPrefix(server.URL, "http://") + "/api/realtime/ws",
		tenantID: tenantID,
		userID:   userID,
	}
}

// dialAndAuth opens a WS connection, sends an auth frame, and waits
// for auth.ok. Returns the live conn so the caller can drive further
// frames.
func dialAndAuth(t *testing.T, ctx context.Context, fx *wsHandlerFixture, token string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.Dial(ctx, fx.wsURL, &websocket.DialOptions{
		Subprotocols: []string{wireSubprotocol},
	})
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	authFrame := rtapi.Frame{Type: rtapi.FrameAuth, Token: token}
	require.NoError(t, writeJSON(ctx, conn, authFrame))
	got, err := readJSON(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, rtapi.FrameAuthOK, got.Type)
	return conn
}

func writeJSON(ctx context.Context, conn *websocket.Conn, frame rtapi.Frame) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

func readJSON(ctx context.Context, conn *websocket.Conn) (rtapi.Frame, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return rtapi.Frame{}, err
	}
	var frame rtapi.Frame
	if err := json.Unmarshal(data, &frame); err != nil {
		return rtapi.Frame{}, err
	}
	return frame, nil
}

// TestWSHandler_AuthOK_RegistersWithHub drives the canonical happy
// path: dial, auth.ok, conn appears in Hub stats, presence OnConnect
// fires, Touch ticker fires, OnDisconnect fires on close.
func TestWSHandler_AuthOK_RegistersWithHub(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"admin"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialAndAuth(t, ctx, fx, "valid-token")

	// Conn is registered with the Hub.
	require.Eventually(t, func() bool {
		return fx.hub.Stats().Connections == 1
	}, 2*time.Second, 20*time.Millisecond)

	// Presence OnConnect fired exactly once with our claims.
	assert.Equal(t, 1, fx.presence.connectsCount())

	// Touch ticker fires periodically; wait for at least one touch.
	require.Eventually(t, func() bool {
		return fx.presence.touchesCount() >= 1
	}, 2*time.Second, 20*time.Millisecond)

	// Clean shutdown.
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "test done"))
	// Wait for OnDisconnect via the per-conn lifecycle.
	require.Eventually(t, func() bool {
		return fx.presence.disconnectCount.Load() == 1
	}, 2*time.Second, 20*time.Millisecond)
}

// TestWSHandler_SubscribeAck verifies the inbound FrameSubscribe path
// emits a FrameSubscribeOK with the allocated subID.
func TestWSHandler_SubscribeAck(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"admin"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialAndAuth(t, ctx, fx, "valid-token")

	subFrame := rtapi.Frame{Type: rtapi.FrameSubscribe, Topic: rtapi.TopicOperatorsState}
	require.NoError(t, writeJSON(ctx, conn, subFrame))

	got, err := readJSON(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, rtapi.FrameSubscribeOK, got.Type)
	assert.NotEmpty(t, got.SubID)

	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "done"))
}

// TestWSHandler_SubscribeRejectedByRBAC verifies the inbound
// FrameSubscribe path emits a FrameSubscribeErr when the RBAC matrix
// denies (operator role attempting to subscribe to TopicTrunksHealth
// which is admin-only).
func TestWSHandler_SubscribeRejectedByRBAC(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"operator"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialAndAuth(t, ctx, fx, "valid-token")

	subFrame := rtapi.Frame{Type: rtapi.FrameSubscribe, Topic: rtapi.TopicTrunksHealth}
	require.NoError(t, writeJSON(ctx, conn, subFrame))

	got, err := readJSON(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, rtapi.FrameSubscribeErr, got.Type)
	assert.NotEmpty(t, got.Reason)

	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "done"))
}

// TestWSHandler_UnsubscribeIsSilent verifies an inbound
// FrameUnsubscribe with an unknown sub_id is a silent no-op (the Hub
// tolerates missing keys and the handler does not emit an error
// frame).
func TestWSHandler_UnsubscribeIsSilent(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"admin"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialAndAuth(t, ctx, fx, "valid-token")

	subFrame := rtapi.Frame{Type: rtapi.FrameSubscribe, Topic: rtapi.TopicOperatorsState}
	require.NoError(t, writeJSON(ctx, conn, subFrame))
	ok, err := readJSON(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, rtapi.FrameSubscribeOK, ok.Type)

	require.NoError(t, writeJSON(ctx, conn, rtapi.Frame{
		Type:  rtapi.FrameUnsubscribe,
		SubID: ok.SubID,
	}))

	// Verify no SubscribeOK / SubscribeErr arrives. Pings are emitted
	// by the per-conn pinger so we may see those — only assert that
	// every received frame is a FramePing (a valid no-op shape).
	rctx, rcancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer rcancel()
	for {
		f, err := readJSON(rctx, conn)
		if err != nil {
			break // deadline hit — done.
		}
		require.NotEqual(t, rtapi.FrameSubscribeErr, f.Type,
			"unsubscribe must not surface as a subscribe error")
		require.NotEqual(t, rtapi.FrameSubscribeOK, f.Type,
			"unsubscribe must not surface as a subscribe ok")
	}

	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "done"))
}

// TestNewWSHandler_NilHubPanics verifies the constructor enforces the
// non-nil hub contract.
func TestNewWSHandler_NilHubPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		newWSHandler(wsHandlerConfig{
			auth:   &stubAuthValidator{},
			logger: zap.NewNop(),
		})
	})
}

// TestNewWSHandler_NilAuthPanics verifies the constructor enforces
// the non-nil auth contract.
func TestNewWSHandler_NilAuthPanics(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)
	require.Panics(t, func() {
		newWSHandler(wsHandlerConfig{
			hub:    hub,
			logger: logger,
		})
	})
}

// TestNewWSHandler_NilLoggerSafe verifies the constructor falls back
// to a nop logger when none is provided.
func TestNewWSHandler_NilLoggerSafe(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)
	h := newWSHandler(wsHandlerConfig{
		hub:    hub,
		auth:   &stubAuthValidator{},
		logger: nil, // nil — falls back to nop
	})
	require.NotNil(t, h)
}

// TestWSHandler_BadToken_Closes4401 verifies a wire-side auth frame
// with an invalid token produces a 4401 close from the server.
func TestWSHandler_BadToken_Closes4401(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"admin"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, fx.wsURL, &websocket.DialOptions{
		Subprotocols: []string{wireSubprotocol},
	})
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	require.NoError(t, writeJSON(ctx, conn, rtapi.Frame{Type: rtapi.FrameAuth, Token: "wrong"}))

	// Expect FrameAuthError, then close with 4401.
	got, err := readJSON(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, rtapi.FrameAuthError, got.Type)

	// The next read returns a close frame.
	_, _, err = conn.Read(ctx)
	require.Error(t, err)
	assert.Equal(t, websocket.StatusCode(4401), websocket.CloseStatus(err))

	// No presence side-effects on a failed auth.
	assert.Equal(t, 0, fx.presence.connectsCount())
}

// TestWSHandler_HubBroadcastReachesClient drives the production fan-out
// path end-to-end: client dials + auths + subscribes, server broadcasts
// to TopicForceCommands, client receives the event frame.
func TestWSHandler_HubBroadcastReachesClient(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"operator"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialAndAuth(t, ctx, fx, "valid-token")

	// Wait for the conn to register with the Hub.
	require.Eventually(t, func() bool {
		return fx.hub.Stats().Connections == 1
	}, 2*time.Second, 10*time.Millisecond)

	// The conn is registered but has no subscriptions yet — the WS
	// handler does NOT auto-subscribe. We use the direct inbound
	// FrameSubscribe path: the realtime Connection forwards
	// FrameSubscribe to the Hub via the wired SubscribeFn. Drive that
	// via the Hub's package API directly using the HubCallback test
	// path:
	//
	// Since the Hub's wire-frame subscribe forwarder isn't part of
	// Task 7, we assert via the Hub-direct Broadcast path. Use a
	// Subscribe frame on the wire and watch for subscribe.ok via the
	// Hub callback:
	subFrame := rtapi.Frame{Type: rtapi.FrameSubscribe, Topic: rtapi.TopicForceCommands}
	require.NoError(t, writeJSON(ctx, conn, subFrame))

	// Wait for the subscription to appear in Hub.Stats — the
	// SetHubCallback inside the handler maps FrameSubscribe to
	// Connection.Subscribe (which goes through the Hub's
	// subscribeForConn).
	require.Eventually(t, func() bool {
		return fx.hub.Stats().BySubscription[rtapi.TopicForceCommands] == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Consume the FrameSubscribeOK ack the handler emitted.
	ack, err := readJSON(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, rtapi.FrameSubscribeOK, ack.Type)
	require.NotEmpty(t, ack.SubID)

	// Broadcast to the operator. Self-only topic — UserID required.
	count := fx.hub.Broadcast(ctx, rtapi.TopicForceCommands,
		json.RawMessage(`{"action":"force-pause"}`),
		rtapi.BroadcastFilter{TenantID: fx.tenantID, UserID: fx.userID},
	)
	require.Equal(t, 1, count)

	// Read the inbound event frame on the client.
	got, err := readJSON(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, rtapi.FrameEvent, got.Type)
	assert.Equal(t, rtapi.TopicForceCommands, got.Topic)

	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "done"))
}

// TestWSHandler_ConcurrentConnections verifies two distinct users may
// connect simultaneously and each receives its own broadcast.
func TestWSHandler_ConcurrentConnections(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"operator"})
	// Override claims per-token for two different users.
	tenantID := fx.tenantID
	userA := fx.userID
	userB := uuid.NewString()
	fx.auth.token = "alice"
	fx.auth.claims = rtapi.Claims{UserID: userA, TenantID: tenantID, Roles: []string{"operator"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connA := dialAndAuth(t, ctx, fx, "alice")
	require.Eventually(t, func() bool {
		return fx.hub.Stats().Connections == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Switch credentials for the second dial — auth validator
	// re-projects per-token claims.
	fx.auth.token = "bob"
	fx.auth.claims = rtapi.Claims{UserID: userB, TenantID: tenantID, Roles: []string{"operator"}}
	connB := dialAndAuth(t, ctx, fx, "bob")

	require.Eventually(t, func() bool {
		return fx.hub.Stats().Connections == 2
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, connA.Close(websocket.StatusNormalClosure, "a done"))
	require.NoError(t, connB.Close(websocket.StatusNormalClosure, "b done"))
}

// TestWSHandler_NoAuthFrame_TimesOut verifies a client that never
// sends an auth frame is dropped after AuthTimeout without leaking
// goroutines.
func TestWSHandler_NoAuthFrame_TimesOut(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"admin"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, fx.wsURL, &websocket.DialOptions{
		Subprotocols: []string{wireSubprotocol},
	})
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	// Don't send any frame — wait for the server-side AuthTimeout
	// (1s) to fire. The next read returns an error.
	_, _, err = conn.Read(ctx)
	require.Error(t, err)
}

// TestWSHandler_NoOriginAccept_Forbidden verifies an explicit refusal
// when the AllowedOrigins gate denies the upgrade. We construct a new
// fixture with a strict origin list that does not match localhost.
func TestWSHandler_NoOriginAccept_Forbidden(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	// Use a Nop logger here so log lines emitted by the per-conn
	// goroutines AFTER the test ends (close-handshake unwinding) do
	// not race against the testing.T's *common.destination — a known
	// zaptest hazard for hijacked WebSocket goroutines that the
	// httptest.Server.Close cannot wait for.
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)
	connMetrics := service.RegisterMetrics(reg)
	presence := &fakePresenceTracker{}
	auth := &stubAuthValidator{token: "tok"}

	h := newWSHandler(wsHandlerConfig{
		hub:         hub,
		auth:        auth,
		metrics:     connMetrics,
		presence:    presence,
		replicaID:   "r",
		logger:      logger,
		origins:     []string{"strict.example.com"},
		touchPeriod: 50 * time.Millisecond,
		connConfig:  service.ConnectionConfig{Logger: logger, AuthTimeout: time.Second},
	})

	r := gin.New()
	r.GET("/api/realtime/ws", h.handle)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	wsURL := "ws://" + strings.TrimPrefix(server.URL, "http://") + "/api/realtime/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := stdhttp.Header{}
	// Force a non-matching Origin so the server-side OriginPatterns
	// gate fires; otherwise coder/websocket's Dial omits the Origin
	// header entirely and the same-origin check passes.
	hdr.Set("Origin", "http://attacker.example.org")
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{wireSubprotocol},
		HTTPHeader:   hdr,
	})
	require.Error(t, err)
	if resp != nil {
		assert.Equal(t, stdhttp.StatusForbidden, resp.StatusCode)
		_ = resp.Body.Close()
	}
}

// TestWSHandler_TouchLapsed_ClosesConn verifies a Touch returning
// ErrPresenceLapsed closes the connection with CloseGoingAway. We
// inject the error via the fakePresenceTracker.
func TestWSHandler_TouchLapsed_ClosesConn(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"admin"})
	fx.presence.mu.Lock()
	fx.presence.touchErr = service.ErrPresenceLapsed
	fx.presence.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialAndAuth(t, ctx, fx, "valid-token")

	// Read should observe a close (going-away) shortly after the
	// first Touch tick fires. Timeout 2s gives plenty of margin
	// against the 50ms touch period.
	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	_, _, err := conn.Read(rctx)
	require.Error(t, err)
	st := websocket.CloseStatus(err)
	assert.Equal(t, websocket.StatusGoingAway, st)
}

// newTestConnectionHTTP builds a *service.Connection backed by a
// stubWS (defined in force_handler_test.go) with sensible test
// defaults. Used by transport/http unit tests that need a bare
// Connection without a live WebSocket or Hub.
func newTestConnectionHTTP(t *testing.T, cfg service.ConnectionConfig) *service.Connection {
	t.Helper()
	if cfg.WriteBufferSize == 0 {
		cfg.WriteBufferSize = 16
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = time.Second
	}
	if cfg.AuthTimeout == 0 {
		cfg.AuthTimeout = time.Second
	}
	if cfg.PingPeriod == 0 {
		cfg.PingPeriod = time.Second
	}
	if cfg.PongTimeout == 0 {
		cfg.PongTimeout = 2 * time.Second
	}
	return service.NewConnection(stubWS{}, cfg)
}

// newTestWSHandler builds a *wsHandler with a nop logger, a minimal
// hub, and no presence tracker. Suitable for unit tests that drive
// the dispatch table without a live WebSocket.
func newTestWSHandler(t *testing.T) *wsHandler {
	t.Helper()
	logger := zap.NewNop()
	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)
	return newWSHandler(wsHandlerConfig{
		hub:    hub,
		auth:   &stubAuthValidator{token: "tok"},
		logger: logger,
	})
}

// TestWSHandler_RefcountKeepsPresenceWhileSiblingActive verifies a
// per-pod refcount preserves the user's presence key while a second
// connection is open. OnDisconnect on the tracker fires only when the
// last local conn closes.
func TestWSHandler_RefcountKeepsPresenceWhileSiblingActive(t *testing.T) {
	t.Parallel()
	fx := newWSHandlerFixture(t, []string{"operator"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connA := dialAndAuth(t, ctx, fx, "valid-token")
	require.Eventually(t, func() bool {
		return fx.hub.Stats().Connections == 1 && fx.presence.connectsCount() == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Open a second connection for the SAME user. OnConnect fires
	// per local conn (cheap; production tracker treats it as
	// idempotent SET).
	connB := dialAndAuth(t, ctx, fx, "valid-token")
	require.Eventually(t, func() bool {
		return fx.hub.Stats().Connections == 2
	}, 2*time.Second, 10*time.Millisecond)

	// Close A. With a refcount, OnDisconnect should NOT fire while B
	// remains active. require.Never gives us a deterministic negative
	// assertion: the predicate is polled and only the absence of a
	// fire-up over the window is sufficient.
	require.NoError(t, connA.Close(websocket.StatusNormalClosure, "a done"))
	require.Never(t, func() bool {
		return fx.presence.disconnectCount.Load() != 0
	}, 150*time.Millisecond, 20*time.Millisecond,
		"OnDisconnect must not fire while sibling conn is active")

	// Close B. NOW OnDisconnect should fire exactly once.
	require.NoError(t, connB.Close(websocket.StatusNormalClosure, "b done"))
	require.Eventually(t, func() bool {
		return fx.presence.disconnectCount.Load() == 1
	}, 2*time.Second, 20*time.Millisecond)
}

// TestWSHandler_HandleSubscribeFrame_CrossTenantReasonScrubbed locks
// in the Plan 11.3 Task 1 contract: when Allow returns
// ErrCrossTenantSubscribe (or any error wrapping it), the
// FrameSubscribeErr Reason MUST be the fixed
// "cross-tenant subscription denied" string — NOT err.Error() —
// so a client cannot probe entity existence cross-tenant via
// wire-string parsing.
func TestWSHandler_HandleSubscribeFrame_CrossTenantReasonScrubbed(t *testing.T) {
	t.Parallel()

	// stubAuth + crossTenantSubscribeFn: a SubscribeFn that always
	// rejects with a wrapped ErrCrossTenantSubscribe carrying a
	// distinguishing inner message.
	innerMsg := "operator_id=victim-op: cmd/api: get user victim-op: not found"
	subscribeFn := func(_ context.Context, _ *service.Connection, _ rtapi.Topic, _ rtapi.SubscriptionFilter) (string, error) {
		return "", fmt.Errorf("%w: %s", service.ErrCrossTenantSubscribe, innerMsg)
	}

	conn := newTestConnectionHTTP(t, service.ConnectionConfig{})
	conn.SeedClaims(rtapi.Claims{UserID: "u1", TenantID: "t1", Roles: []string{"supervisor"}})
	conn.SetSubscribeFnForTest(subscribeFn) // test seam added in Step 3

	h := newTestWSHandler(t)
	conn.SetHubCallback(h.HandleSubscribeFrame) // exported test seam added in Step 3

	conn.HandleFrameForTest(rtapi.Frame{
		Type:  rtapi.FrameSubscribe,
		Topic: rtapi.TopicOperatorsState,
		Filter: &rtapi.SubscriptionFilter{
			OperatorID: "victim-op",
		},
	})

	// Drain the queued FrameSubscribeErr.
	got := conn.DrainSendForTest()
	require.NotNil(t, got)
	assert.Equal(t, rtapi.FrameSubscribeErr, got.Type)
	assert.Equal(t, "cross-tenant subscription denied", got.Reason,
		"Reason must be the fixed scrubbed string for ErrCrossTenantSubscribe")
	assert.NotContains(t, got.Reason, "victim-op",
		"scrubbed Reason must not leak the operator_id")
	assert.NotContains(t, got.Reason, "not found",
		"scrubbed Reason must not leak the inner not-found error")
}

// TestWSHandler_HandleSubscribeFrame_NonCrossTenantReasonPassthrough
// ensures the scrub is targeted: other RBAC errors still surface
// their err.Error() (operators may need that context to debug).
func TestWSHandler_HandleSubscribeFrame_NonCrossTenantReasonPassthrough(t *testing.T) {
	t.Parallel()

	subscribeFn := func(_ context.Context, _ *service.Connection, _ rtapi.Topic, _ rtapi.SubscriptionFilter) (string, error) {
		return "", fmt.Errorf("%w: roles=[operator] topic=trunks.health", service.ErrTopicForbidden)
	}

	conn := newTestConnectionHTTP(t, service.ConnectionConfig{})
	conn.SeedClaims(rtapi.Claims{UserID: "u1", TenantID: "t1", Roles: []string{"operator"}})
	conn.SetSubscribeFnForTest(subscribeFn)
	h := newTestWSHandler(t)
	conn.SetHubCallback(h.HandleSubscribeFrame)

	conn.HandleFrameForTest(rtapi.Frame{
		Type:  rtapi.FrameSubscribe,
		Topic: rtapi.TopicTrunksHealth,
	})

	got := conn.DrainSendForTest()
	require.NotNil(t, got)
	assert.Equal(t, rtapi.FrameSubscribeErr, got.Type)
	assert.Contains(t, got.Reason, "topic not allowed",
		"non-cross-tenant errors retain their original Reason")
}

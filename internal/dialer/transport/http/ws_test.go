package http_test

import (
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	transporthttp "github.com/sociopulse/platform/internal/dialer/transport/http"
)

// wsFixture wraps the standard fixture with an httptest.Server so WS
// upgrades can complete. We override AllowedOrigins=["*"] because
// httptest's hostname (127.0.0.1:NNNNN) does not match coder/websocket's
// default same-origin check.
type wsFixture struct {
	server *httptest.Server
	fsm    *fakeFSM
	rt     *fakeRouter
	val    *fakeValidator
	pub    *fakePubSub

	tenantID uuid.UUID
	userID   uuid.UUID
}

func newWSFixture(t *testing.T) *wsFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	val := &fakeValidator{
		claims: authapi.Claims{
			UserID:   userID,
			TenantID: tenantID,
			Login:    "alice",
			Roles:    []authapi.Role{authapi.RoleOperator},
		},
	}
	wf := &wsFixture{
		fsm:      &fakeFSM{},
		rt:       &fakeRouter{},
		val:      val,
		pub:      newFakePubSub(),
		tenantID: tenantID,
		userID:   userID,
	}
	r := gin.New()
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{
		FSM:                wf.fsm,
		Router:             wf.rt,
		Validator:          wf.val,
		RBAC:               fakeRBAC{},
		SnapshotPubSub:     wf.pub,
		CallTenantResolver: newFakeCallTenantResolver(),
		Logger:             nil,
		WSConfig: transporthttp.WSConfig{
			// Tighten the timings so the test does not take 30s.
			PingPeriod:     50 * time.Millisecond,
			PongTimeout:    500 * time.Millisecond,
			WriteTimeout:   500 * time.Millisecond,
			AllowedOrigins: []string{"*"},
		},
	})
	wf.server = httptest.NewServer(r)
	t.Cleanup(wf.server.Close)
	return wf
}

func (wf *wsFixture) wsURL(path string) string {
	// httptest.Server URLs are http://127.0.0.1:NNNNN — swap to ws://.
	return "ws://" + strings.TrimPrefix(wf.server.URL, "http://") + path
}

// readSnapshot reads exactly one frame and decodes it as SnapshotDTO.
func readSnapshot(t *testing.T, ctx context.Context, c *websocket.Conn) transporthttp.SnapshotDTO {
	t.Helper()
	typ, data, err := c.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, typ)
	var out transporthttp.SnapshotDTO
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

// =============================================================================
// Tests
// =============================================================================

func TestWS_HappyPath_ThreeSnapshots(t *testing.T) {
	t.Parallel()
	wf := newWSFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wf.wsURL("/api/operator/ws?token=abc"), nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	// Wait until the server-side Subscribe registered. Publish drops
	// silently when there is no subscriber yet, so we poll.
	pid := uuid.New()
	snap1 := dialerapi.Snapshot{
		State:          dialerapi.StateReady,
		StateEnteredAt: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		ProjectID:      &pid,
		HeartbeatAt:    time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if wf.pub.Publish(wf.tenantID, wf.userID, snap1) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	got1 := readSnapshot(t, ctx, conn)
	assert.Equal(t, "ready", got1.State)
	require.NotNil(t, got1.ProjectID)
	assert.Equal(t, pid, *got1.ProjectID)

	pause := "bio_break"
	snap2 := dialerapi.Snapshot{
		State:          dialerapi.StatePause,
		StateEnteredAt: time.Date(2026, 5, 1, 9, 5, 0, 0, time.UTC),
		PauseReason:    &pause,
		HeartbeatAt:    time.Date(2026, 5, 1, 9, 5, 0, 0, time.UTC),
	}
	require.True(t, wf.pub.Publish(wf.tenantID, wf.userID, snap2))
	got2 := readSnapshot(t, ctx, conn)
	assert.Equal(t, "pause", got2.State)
	require.NotNil(t, got2.PauseReason)
	assert.Equal(t, "bio_break", *got2.PauseReason)

	cid := uuid.New()
	rid := uuid.New()
	snap3 := dialerapi.Snapshot{
		State:          dialerapi.StateCall,
		StateEnteredAt: time.Date(2026, 5, 1, 9, 10, 0, 0, time.UTC),
		CurrentCallID:  &cid,
		RespondentID:   &rid,
		HeartbeatAt:    time.Date(2026, 5, 1, 9, 10, 0, 0, time.UTC),
	}
	require.True(t, wf.pub.Publish(wf.tenantID, wf.userID, snap3))
	got3 := readSnapshot(t, ctx, conn)
	assert.Equal(t, "call", got3.State)
	require.NotNil(t, got3.CurrentCallID)
	assert.Equal(t, cid, *got3.CurrentCallID)
	require.NotNil(t, got3.RespondentID)
	assert.Equal(t, rid, *got3.RespondentID)

	// Clean shutdown.
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "test done"))
}

func TestWS_NoToken_Unauthorized(t *testing.T) {
	t.Parallel()
	wf := newWSFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wf.wsURL("/api/operator/ws"), nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, stdhttp.StatusUnauthorized, resp.StatusCode)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func TestWS_InvalidToken_Unauthorized(t *testing.T) {
	t.Parallel()
	wf := newWSFixture(t)
	wf.val.err = authapi.ErrTokenInvalid

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wf.wsURL("/api/operator/ws?token=bad"), nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, stdhttp.StatusUnauthorized, resp.StatusCode)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func TestWS_RevokedToken_Unauthorized(t *testing.T) {
	t.Parallel()
	wf := newWSFixture(t)
	wf.val.err = authapi.ErrTokenRevoked

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wf.wsURL("/api/operator/ws?token=stale"), nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, stdhttp.StatusUnauthorized, resp.StatusCode)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func TestWS_NoRoles_Forbidden(t *testing.T) {
	t.Parallel()
	wf := newWSFixture(t)
	wf.val.claims.Roles = nil // strip every role

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wf.wsURL("/api/operator/ws?token=abc"), nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, stdhttp.StatusForbidden, resp.StatusCode)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func TestWS_BearerHeaderFallback(t *testing.T) {
	t.Parallel()
	wf := newWSFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := stdhttp.Header{}
	hdr.Set("Authorization", "Bearer foo")
	conn, resp, err := websocket.Dial(ctx, wf.wsURL("/api/operator/ws"),
		&websocket.DialOptions{HTTPHeader: hdr})
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	pid := uuid.New()
	snap := dialerapi.Snapshot{
		State:          dialerapi.StateReady,
		StateEnteredAt: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		ProjectID:      &pid,
		HeartbeatAt:    time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if wf.pub.Publish(wf.tenantID, wf.userID, snap) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := readSnapshot(t, ctx, conn)
	assert.Equal(t, "ready", got.State)
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
}

// TestWS_ChannelClosed_ServerClosesGracefully verifies the WS pump
// closes the connection when SnapshotPubSub closes the subscription
// channel — exercising the !ok branch in pumpSnapshots and the
// closeGracefully helper.
func TestWS_ChannelClosed_ServerClosesGracefully(t *testing.T) {
	t.Parallel()
	wf := newWSFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wf.wsURL("/api/operator/ws?token=abc"), nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	// Wait until subscribed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		wf.pub.mu.Lock()
		got := len(wf.pub.subs)
		wf.pub.mu.Unlock()
		if got == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Force the subscription channel closed from the server side.
	wf.pub.mu.Lock()
	for k, ch := range wf.pub.subs {
		close(ch)
		delete(wf.pub.subs, k)
	}
	wf.pub.mu.Unlock()

	// Read should return a normal close.
	_, _, err = conn.Read(ctx)
	require.Error(t, err)
	// Status code should be normal closure.
	st := websocket.CloseStatus(err)
	assert.Equal(t, websocket.StatusNormalClosure, st)
}

// TestWS_PingPong_StaysAlive verifies the server-side Ping ticker
// fires and the client's Pong keeps the connection healthy. We hold
// the connection open for ~5 ping intervals; if the server crashed
// or shut down the conn we'd see an error on the next read after the
// channel-close trigger.
func TestWS_PingPong_StaysAlive(t *testing.T) {
	t.Parallel()
	wf := newWSFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wf.wsURL("/api/operator/ws?token=abc"), nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	// Wait long enough for several pings (PingPeriod=50ms, so 300ms = 6 cycles).
	time.Sleep(300 * time.Millisecond)

	// Connection is still alive — push a snapshot to verify.
	pid := uuid.New()
	snap := dialerapi.Snapshot{
		State:          dialerapi.StateReady,
		StateEnteredAt: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		ProjectID:      &pid,
		HeartbeatAt:    time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
	}
	require.True(t, wf.pub.Publish(wf.tenantID, wf.userID, snap))
	got := readSnapshot(t, ctx, conn)
	assert.Equal(t, "ready", got.State)
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
}

// TestWSConfig_ResolvedDefaults exercises the zero-value fallback in
// WSConfig.resolved() via a fresh fixture that omits WSConfig.
func TestWSConfig_ResolvedDefaults(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	val := &fakeValidator{
		claims: authapi.Claims{
			UserID: userID, TenantID: tenantID, Login: "alice",
			Roles: []authapi.Role{authapi.RoleOperator},
		},
	}
	pub := newFakePubSub()
	r := gin.New()
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{
		FSM:                &fakeFSM{},
		Router:             &fakeRouter{},
		Validator:          val,
		RBAC:               fakeRBAC{},
		SnapshotPubSub:     pub,
		CallTenantResolver: newFakeCallTenantResolver(),
		Logger:             nil,
		// WSConfig left at zero value to exercise resolved() defaults.
		// We override AllowedOrigins to "*" so the handshake succeeds
		// from httptest's host. PingPeriod / PongTimeout / WriteTimeout
		// stay zero.
		WSConfig: transporthttp.WSConfig{AllowedOrigins: []string{"*"}},
	})
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsurl := "ws://" + strings.TrimPrefix(server.URL, "http://") + "/api/operator/ws?token=abc"

	conn, resp, err := websocket.Dial(ctx, wsurl, nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	// Wait for subscribe.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pub.mu.Lock()
		got := len(pub.subs)
		pub.mu.Unlock()
		if got == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pid := uuid.New()
	snap := dialerapi.Snapshot{
		State:          dialerapi.StateReady,
		StateEnteredAt: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		ProjectID:      &pid,
		HeartbeatAt:    time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
	}
	require.True(t, pub.Publish(tenantID, userID, snap))
	got := readSnapshot(t, ctx, conn)
	assert.Equal(t, "ready", got.State)
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
}

// TestWS_WriteAfterClientDeath_LogsAndExits verifies the WS handler
// logs and exits cleanly when a snapshot publish races with a client
// network disconnect. The fake network closes the conn immediately;
// the next snapshot Publish will fail to write and exercise the
// logWSError code path.
func TestWS_WriteAfterClientDeath_LogsAndExits(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	val := &fakeValidator{
		claims: authapi.Claims{
			UserID: userID, TenantID: tenantID, Login: "alice",
			Roles: []authapi.Role{authapi.RoleOperator},
		},
	}
	pub := newFakePubSub()
	r := gin.New()
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{
		FSM:                &fakeFSM{},
		Router:             &fakeRouter{},
		Validator:          val,
		RBAC:               fakeRBAC{},
		SnapshotPubSub:     pub,
		CallTenantResolver: newFakeCallTenantResolver(),
		Logger:             zap.NewNop(), // non-nil to exercise logWSError
		WSConfig: transporthttp.WSConfig{
			PingPeriod:     50 * time.Millisecond,
			PongTimeout:    500 * time.Millisecond,
			WriteTimeout:   500 * time.Millisecond,
			AllowedOrigins: []string{"*"},
		},
	})
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsurl := "ws://" + strings.TrimPrefix(server.URL, "http://") + "/api/operator/ws?token=abc"

	conn, resp, err := websocket.Dial(ctx, wsurl, nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	// Wait until subscribed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pub.mu.Lock()
		got := len(pub.subs)
		pub.mu.Unlock()
		if got == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Hard close from the client without WS handshake — the server's
	// next attempt to write a snapshot will fail. CloseNow drops the
	// underlying connection without sending a close frame.
	require.NoError(t, conn.CloseNow())

	// Publish enough snapshots to overflow the 16-buffer and force a
	// write attempt. A buffered Publish that doesn't drop will sit in
	// the channel; we need the pump to dequeue and try writing.
	for range 30 {
		pub.Publish(tenantID, userID, dialerapi.Snapshot{
			State:          dialerapi.StateReady,
			StateEnteredAt: time.Now().UTC(),
			HeartbeatAt:    time.Now().UTC(),
		})
	}

	// Wait for the server-side cleanup. We poll because the cancel is
	// asynchronous (runs on the WS handler's request goroutine).
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pub.mu.Lock()
		left := len(pub.subs)
		pub.mu.Unlock()
		if left == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	pub.mu.Lock()
	left := len(pub.subs)
	pub.mu.Unlock()
	t.Fatalf("subscriber slot was not released after client hard-close; %d subs remain", left)
}

// TestWS_BadOrigin_Rejected verifies that without AllowedOrigins=["*"]
// the websocket.Accept rejects a cross-origin handshake. We mount with
// the default config (empty AllowedOrigins, same-origin only) and dial
// with a forced Origin header.
func TestWS_BadOrigin_Rejected(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	val := &fakeValidator{
		claims: authapi.Claims{
			UserID: userID, TenantID: tenantID, Login: "alice",
			Roles: []authapi.Role{authapi.RoleOperator},
		},
	}
	pub := newFakePubSub()
	r := gin.New()
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{
		FSM:                &fakeFSM{},
		Router:             &fakeRouter{},
		Validator:          val,
		RBAC:               fakeRBAC{},
		SnapshotPubSub:     pub,
		CallTenantResolver: newFakeCallTenantResolver(),
		Logger:             nil,
		// No AllowedOrigins → coder/websocket enforces same-origin.
	})
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsurl := "ws://" + strings.TrimPrefix(server.URL, "http://") + "/api/operator/ws?token=abc"

	hdr := stdhttp.Header{}
	hdr.Set("Origin", "http://evil.example.com")
	conn, resp, err := websocket.Dial(ctx, wsurl, &websocket.DialOptions{HTTPHeader: hdr})
	require.Error(t, err)
	if conn != nil {
		_ = conn.CloseNow()
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
}

// TestWS_SubscribeCancelsOnClientClose verifies the WS handler
// releases its Subscribe slot when the client closes the connection
// — a leak in the fake's `subs` map would indicate the handler did
// not call cancel().
func TestWS_SubscribeCancelsOnClientClose(t *testing.T) {
	t.Parallel()
	wf := newWSFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wf.wsURL("/api/operator/ws?token=abc"), nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	// Wait until subscribed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		wf.pub.mu.Lock()
		got := len(wf.pub.subs)
		wf.pub.mu.Unlock()
		if got == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "bye"))

	// Wait for the server-side cleanup. We poll because the cancel is
	// asynchronous (runs on the WS handler's request goroutine).
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		wf.pub.mu.Lock()
		left := len(wf.pub.subs)
		wf.pub.mu.Unlock()
		if left == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	wf.pub.mu.Lock()
	left := len(wf.pub.subs)
	wf.pub.mu.Unlock()
	t.Fatalf("subscriber slot was not released; %d subs remain", left)
}

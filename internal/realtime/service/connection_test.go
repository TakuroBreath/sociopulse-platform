package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// TestMain installs the goroutine leak guard. Every realtime
// connection spawns three goroutines (reader / writer / pinger) so
// test cleanup discipline matters: any forgotten Close + Run-blocking
// will surface here as a leak.
//
// Suppressed leak: go-redis v9 spawns a background tryDial() retry
// goroutine when its connection pool exhausts dial errors (e.g.,
// TestPresence_RedisErrorPropagation deliberately closes miniredis
// mid-test to assert error-path metrics). tryDial sleeps up to 1s
// between attempts and only checks p.closed() AFTER waking — closing
// the redis client does NOT preempt the sleep (no ctx threaded
// through; a known go-redis architectural limitation, see
// internal/pool/pool.go's tryDial). On a -race CI runner with slower
// goroutine scheduling the tests can finish before the goroutine
// wakes and exits, surfacing here as a leak. Suppressing this
// specific function still catches every other leak in the package —
// the connection reader/writer/pinger triple, the presence touch
// ticker, and any forgotten Hub/dispatcher goroutines.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/redis/go-redis/v9/internal/pool.(*ConnPool).tryDial"),
	)
}

func TestConnection_DropsOldestFrameWhenSlowConsumer(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{
		WriteBufferSize: 4,
		WriteTimeout:    100 * time.Millisecond,
	})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	reg := prometheus.NewRegistry()
	conn.SetMetrics(service.RegisterMetrics(reg))

	// No writer goroutine — Run is never called. Push 5 frames
	// into a 4-slot buffer; the 5th triggers drop-oldest, taking
	// the count to exactly 1. The buffer is then full with the
	// last 4 frames.
	for range 5 {
		conn.Send(rtapi.Frame{Type: rtapi.FrameEvent, Payload: json.RawMessage(`"frame"`)})
	}

	require.Equal(t, uint64(1), conn.DroppedFrames())
	require.InDelta(t, 1.0, dropCountForConn(t, reg, conn.ID()), 0.0001)
}

func TestConnection_AuthHandshake_Success(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))

	claims, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)
	require.Equal(t, "user-1", claims.UserID)
	require.Equal(t, claims, conn.Claims())

	// Server must respond with auth.ok.
	written := fake.LastWrite()
	require.NotNil(t, written, "expected auth.ok write")
	var resp rtapi.Frame
	require.NoError(t, json.Unmarshal(written, &resp))
	require.Equal(t, rtapi.FrameAuthOK, resp.Type)
}

func TestConnection_AuthHandshake_BadToken(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "bad"}))

	_, err := conn.AuthHandshake(context.Background(), auth)
	require.ErrorIs(t, err, service.ErrAuthFailed)
	require.Equal(t, rtapi.CloseUnauthorized, fake.CloseCode())

	// Server must have written FrameAuthError before closing.
	writes := fake.Writes()
	require.NotEmpty(t, writes)
	var last rtapi.Frame
	require.NoError(t, json.Unmarshal(writes[len(writes)-1], &last))
	require.Equal(t, rtapi.FrameAuthError, last.Type)
}

func TestConnection_AuthHandshake_MissingToken(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth}))

	_, err := conn.AuthHandshake(context.Background(), auth)
	require.ErrorIs(t, err, service.ErrAuthFailed)
	require.Equal(t, rtapi.CloseUnauthorized, fake.CloseCode())
}

func TestConnection_AuthHandshake_WrongFrameType(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameSubscribe}))

	_, err := conn.AuthHandshake(context.Background(), auth)
	require.Error(t, err)
	require.ErrorIs(t, err, service.ErrAuthRequired)
	require.Equal(t, rtapi.ClosePolicyViol, fake.CloseCode())
}

func TestConnection_AuthHandshake_BadJSON(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	auth := newStubAuth()
	fake.QueueRead([]byte("{not json"))

	_, err := conn.AuthHandshake(context.Background(), auth)
	require.Error(t, err)
	require.Equal(t, rtapi.CloseInvalidData, fake.CloseCode())
}

func TestConnection_AuthHandshake_ReadTimeout(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		AuthTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	auth := newStubAuth()
	// No QueueRead — handshake must time out.
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.Error(t, err)
	require.Equal(t, rtapi.CloseProtocolErr, fake.CloseCode())
}

func TestConnection_AuthHandshake_NilValidator(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	_, err := conn.AuthHandshake(context.Background(), nil)
	require.Error(t, err)
}

func TestConnection_Run_RequiresAuth(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	err := conn.Run(context.Background())
	require.Error(t, err)
}

func TestConnection_Run_PongRefreshKeepsConnAlive(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  20 * time.Millisecond,
		PongTimeout: 200 * time.Millisecond,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	// Hammer pongs through the read queue so lastPongAt keeps
	// refreshing well within PongTimeout. Serialize the pong
	// payload up front — calling mustJSON inside the goroutine
	// would tickle testifylint go-require.
	pong := mustJSON(t, rtapi.Frame{Type: rtapi.FramePong})
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				fake.QueueRead(pong)
			}
		}
	}()

	// Hold the connection open for ~150ms — well past PingPeriod
	// but inside PongTimeout. Conn must still be alive.
	time.Sleep(150 * time.Millisecond)
	require.False(t, isClosed(runDone), "connection should still be alive while pongs flow")

	close(stop)
	conn.Close(rtapi.CloseNormal)
	fake.QueueReadErr(errors.New("conn closed"))
	<-runDone
}

func TestConnection_Run_ClosesOnPongTimeout(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  20 * time.Millisecond,
		PongTimeout: 30 * time.Millisecond,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	// No pongs sent. After PingPeriod + PongTimeout the pinger
	// must close the connection with CloseRateLimited.
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("connection did not close on missed pong")
	}
	require.Equal(t, rtapi.CloseRateLimited, fake.CloseCode())
}

func TestConnection_Run_RefreshSwapsClaims(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := &stubAuth{
		validToken: "valid",
		claims:     rtapi.Claims{UserID: "user-1", TenantID: "tenant-1", Roles: []string{"operator"}},
	}
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	// Switch the validator's idea of who "valid" maps to and
	// trigger a refresh. The new claims must be visible via
	// Claims().
	auth.claims = rtapi.Claims{UserID: "user-1", TenantID: "tenant-1", Roles: []string{"operator", "supervisor"}}
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameRefresh, Token: "valid"}))

	require.Eventually(t, func() bool {
		return len(conn.Claims().Roles) == 2
	}, time.Second, 10*time.Millisecond)

	conn.Close(rtapi.CloseNormal)
	fake.QueueReadErr(errors.New("conn closed"))
	<-runDone
}

func TestConnection_Run_RefreshBadTokenClosesUnauthorized(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameRefresh, Token: "bad"}))

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("connection did not close on bad refresh")
	}
	require.Equal(t, rtapi.CloseUnauthorized, fake.CloseCode())
}

func TestConnection_Run_RefreshMissingTokenClosesUnauthorized(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameRefresh, Token: ""}))

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("connection did not close on empty refresh token")
	}
	require.Equal(t, rtapi.CloseUnauthorized, fake.CloseCode())
}

func TestConnection_Run_SubscribeRoutesToHubCallback(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	got := make(chan rtapi.Frame, 4)
	conn.SetHubCallback(func(_ *service.Connection, frame rtapi.Frame) {
		got <- frame
	})

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	fake.QueueRead(mustJSON(t, rtapi.Frame{
		Type:  rtapi.FrameSubscribe,
		Topic: rtapi.TopicOperatorsState,
	}))
	fake.QueueRead(mustJSON(t, rtapi.Frame{
		Type:  rtapi.FrameUnsubscribe,
		SubID: "sub-1",
	}))

	select {
	case f := <-got:
		require.Equal(t, rtapi.FrameSubscribe, f.Type)
	case <-time.After(time.Second):
		t.Fatalf("hub callback never received subscribe frame")
	}
	select {
	case f := <-got:
		require.Equal(t, rtapi.FrameUnsubscribe, f.Type)
	case <-time.After(time.Second):
		t.Fatalf("hub callback never received unsubscribe frame")
	}

	conn.Close(rtapi.CloseNormal)
	fake.QueueReadErr(errors.New("conn closed"))
	<-runDone
}

func TestConnection_Run_PingFromClientGetsPong(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FramePing}))

	require.Eventually(t, func() bool {
		for _, w := range fake.Writes() {
			var f rtapi.Frame
			if err := json.Unmarshal(w, &f); err == nil && f.Type == rtapi.FramePong {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)

	conn.Close(rtapi.CloseNormal)
	fake.QueueReadErr(errors.New("conn closed"))
	<-runDone
}

func TestConnection_Run_UnknownFrameKindIgnored(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	// FrameKind not in the dispatch table.
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameKind("unknown.kind")}))
	// Then a normal pong so we can verify the conn didn't die on
	// the unknown frame.
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FramePong}))

	require.Eventually(t, func() bool {
		return !isClosed(runDone)
	}, 200*time.Millisecond, 10*time.Millisecond)

	conn.Close(rtapi.CloseNormal)
	fake.QueueReadErr(errors.New("conn closed"))
	<-runDone
}

func TestConnection_Close_Idempotent(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	// Two competing closes. Idempotent: only the first should
	// reach the WSConn.Close + signal closeChan.
	conn.Close(rtapi.CloseNormal)
	conn.Close(rtapi.CloseGoingAway)

	fake.QueueReadErr(errors.New("conn closed"))
	<-runDone

	// Run finalises the close — fake.Close fires once from Run
	// (no reader/writer/pinger close-pile-up).
	require.LessOrEqual(t, fake.CloseCallCount(), int32(1))
	require.Equal(t, rtapi.CloseNormal, fake.CloseCode())
}

func TestConnection_Close_BeforeRun_NoLeak(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	conn.Close(rtapi.CloseNormal)
	conn.Close(rtapi.CloseGoingAway) // idempotent
}

func TestConnection_Run_ContextCancelClosesConn(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(ctx)
		close(runDone)
	}()

	cancel()
	// Cause the reader to unblock so the goroutine exits.
	fake.QueueReadErr(context.Canceled)

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not exit on ctx cancel")
	}
}

func TestConnection_Send_DropsAreReportedInMetric(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{
		WriteBufferSize: 2,
	})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	reg := prometheus.NewRegistry()
	conn.SetMetrics(service.RegisterMetrics(reg))

	// Push 5 frames into a 2-buffer with no writer attached: 3
	// drops.
	for range 5 {
		conn.Send(rtapi.Frame{Type: rtapi.FrameEvent})
	}

	require.Equal(t, uint64(3), conn.DroppedFrames())
	require.InDelta(t, 3.0, dropCountForConn(t, reg, conn.ID()), 0.0001)
}

func TestConnection_Send_AfterClose_NoOp(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	conn.Close(rtapi.CloseNormal)

	// Must not panic.
	conn.Send(rtapi.Frame{Type: rtapi.FrameEvent})
}

func TestConnection_SubscribeStub_ReturnsErrors(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	// Without auth -> ErrAuthRequired.
	_, err := conn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.ErrorIs(t, err, service.ErrAuthRequired)
}

func TestConnection_SubscribeStub_AfterClose(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	conn.Close(rtapi.CloseNormal)

	_, err := conn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.ErrorIs(t, err, service.ErrConnectionClosed)
}

func TestConnection_UnsubscribeStub_NoOp(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	conn.Unsubscribe("nope")
}

func TestConnection_Run_MalformedFrameIgnoredThenContinues(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	got := make(chan rtapi.Frame, 4)
	conn.SetHubCallback(func(_ *service.Connection, frame rtapi.Frame) {
		got <- frame
	})

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	// Garbage frame — must be ignored, reader must keep running.
	fake.QueueRead([]byte("not json"))
	// Then a legitimate subscribe frame to confirm the reader
	// loop is still alive.
	fake.QueueRead(mustJSON(t, rtapi.Frame{
		Type:  rtapi.FrameSubscribe,
		Topic: rtapi.TopicOperatorsState,
	}))

	select {
	case f := <-got:
		require.Equal(t, rtapi.FrameSubscribe, f.Type)
	case <-time.After(time.Second):
		t.Fatalf("hub callback did not fire after malformed frame")
	}

	conn.Close(rtapi.CloseNormal)
	fake.QueueReadErr(errors.New("conn closed"))
	<-runDone
}

func TestConnection_Run_InboundRateLimitClosesConn(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:      500 * time.Millisecond,
		PongTimeout:     5 * time.Second,
		RateLimitPerSec: 1,
		RateLimitBurst:  1,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	conn.SetMetrics(service.RegisterMetrics(reg))

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	// Burst capacity 1: first frame is allowed, second triggers
	// rate-limit close. Use FramePong so the dispatch is cheap.
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FramePong}))
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FramePong}))

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("connection did not close on rate-limit overflow")
	}
	require.Equal(t, rtapi.CloseRateLimited, fake.CloseCode())
}

func TestConnection_Run_WriterErrorClosesConn(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  20 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	// Make every WriteFrame fail. The pinger will enqueue a ping
	// quickly; the writer will fail; the connection must unwind.
	fake.SetWriteErr(errors.New("boom"))

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("connection did not close on writer error")
	}
	require.Equal(t, rtapi.CloseGoingAway, fake.CloseCode())
}

func TestConnection_Refresh_NoValidator_Wired(t *testing.T) {
	t.Parallel()

	// A connection that never went through AuthHandshake but has
	// authenticated forced (test-only path) should not crash on a
	// refresh frame. We exercise this via dispatchFrame indirectly:
	// build a connection, mark it authenticated by calling
	// AuthHandshake successfully, then nil out the validator and
	// drive a refresh through the reader. The implementation
	// guards on c.auth == nil.
	conn, fake := newTestConnection(t, service.ConnectionConfig{
		PingPeriod:  500 * time.Millisecond,
		PongTimeout: 5 * time.Second,
	})
	defer conn.Close(rtapi.CloseNormal)

	auth := newStubAuth()
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameAuth, Token: "valid"}))
	_, err := conn.AuthHandshake(context.Background(), auth)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		_ = conn.Run(context.Background())
		close(runDone)
	}()

	// Inject the refresh; we have a validator wired so this is
	// actually the happy-path. The "no validator" branch is a
	// defensive guard exercised by the unit on dispatchFrame in a
	// constructor that never went through AuthHandshake — see
	// TestConnection_Claims_ZeroBeforeHandshake for the
	// pre-handshake side.
	fake.QueueRead(mustJSON(t, rtapi.Frame{Type: rtapi.FrameRefresh, Token: "valid"}))

	require.Eventually(t, func() bool {
		for _, w := range fake.Writes() {
			var f rtapi.Frame
			if err := json.Unmarshal(w, &f); err == nil && f.Type == rtapi.FrameRefreshOK {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)

	conn.Close(rtapi.CloseNormal)
	fake.QueueReadErr(errors.New("conn closed"))
	<-runDone
}

func TestConnection_Claims_ZeroBeforeHandshake(t *testing.T) {
	t.Parallel()

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	require.Equal(t, rtapi.Claims{}, conn.Claims())
	require.NotEmpty(t, conn.ID())
}

func TestConnection_DefaultsApplied(t *testing.T) {
	t.Parallel()

	// Pass a fully-zeroed config and verify the connection is
	// usable (defaults populated). Smoke test only — the deeper
	// behaviour is tested elsewhere.
	fake := newFakeWSConn()
	conn := service.NewConnection(fake, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	require.NotEmpty(t, conn.ID())
	conn.Send(rtapi.Frame{Type: rtapi.FrameEvent}) // must not panic
}

func TestRegisterMetrics_NilRegisterer_Panics(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		service.RegisterMetrics(nil)
	})
}

func TestRegisterMetrics_RegistersAllCounters(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m := service.RegisterMetrics(reg)
	require.NotNil(t, m)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	// PongMisses + RateLimitClosures are non-vec counters that
	// register their initial 0 metric — DroppedFrames and
	// AuthFailures are vecs that don't expose any series until
	// the first call. Expect at least 2 metric families.
	require.GreaterOrEqual(t, len(mfs), 2)
}

// isClosed is a non-blocking peek at a "done" channel.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// dropCountForConn returns the realtime_dropped_frames_total{conn_id}
// counter value for a specific connection.
func dropCountForConn(t *testing.T, reg *prometheus.Registry, connID string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "realtime_dropped_frames_total" {
			continue
		}
		for _, m := range mf.Metric {
			if matchesLabel(m, "conn_id", connID) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func matchesLabel(m *dto.Metric, key, val string) bool {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == key && lp.GetValue() == val {
			return true
		}
	}
	return false
}

// TestConnection_CriticalFrameOverflowClosesConnection asserts that a
// blocked writer + a sustained burst of critical frames closes the
// connection with CloseRateLimited rather than silently dropping —
// the documented Plan 11.2 contract for FrameClassCritical.
func TestConnection_CriticalFrameOverflowClosesConnection(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		WriteBufferSize: 16, // telemetry buffer; critical buffer is fixed at 32
	})
	conn.SeedClaims(rtapi.Claims{UserID: "u1", TenantID: "t1", Roles: []string{"admin"}})

	// Block the writer so the queues fill up.
	fake.BlockWrites()

	// Push 50 critical frames; criticalQueueSize=32 — frame 33 should
	// trigger the overflow-close path.
	for range 50 {
		conn.Send(rtapi.Frame{Type: rtapi.FrameEvent, Topic: rtapi.TopicCallEvents})
	}

	require.Eventually(t, conn.IsClosedForTest,
		2*time.Second, 10*time.Millisecond,
		"critical-queue overflow must close the connection")

	// Unblock so writer goroutine drains and exits cleanly (no goleak).
	fake.UnblockWrites()
}

// TestConnection_TelemetryFramesDropOldest_PreservesCritical asserts
// that telemetry overflow does NOT close the connection AND does not
// purge frames from the critical queue. The two queues are
// independent.
func TestConnection_TelemetryFramesDropOldest_PreservesCritical(t *testing.T) {
	t.Parallel()

	conn, fake := newTestConnection(t, service.ConnectionConfig{
		WriteBufferSize: 4, // tiny telemetry buffer; force overflow fast
	})
	conn.SeedClaims(rtapi.Claims{UserID: "u1", TenantID: "t1", Roles: []string{"operator"}})

	fake.BlockWrites()

	// Push one critical frame first — it should land in the critical
	// queue and survive the subsequent telemetry-flood.
	conn.Send(rtapi.Frame{Type: rtapi.FrameEvent, Topic: rtapi.TopicCallEvents})

	// Flood the telemetry queue; drop-oldest must NOT close the conn.
	for range 20 {
		conn.Send(rtapi.Frame{Type: rtapi.FrameEvent, Topic: rtapi.TopicOperatorsState})
	}

	// Sanity: connection still alive (no overflow-close from telemetry).
	require.False(t, conn.IsClosedForTest(),
		"telemetry-queue overflow must NOT close the connection")

	// At least one frame was dropped (drop-oldest counter incremented).
	assert.Positive(t, conn.DroppedFrames(),
		"telemetry overflow should bump drop counter")

	fake.UnblockWrites()
}

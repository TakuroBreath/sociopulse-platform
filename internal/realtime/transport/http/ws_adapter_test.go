package http

import (
	"context"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// startEchoWS spins up an httptest.Server whose handler accepts a
// coder/websocket connection and hands the underlying *websocket.Conn
// to fn. AllowedOrigins is "*" so the test client's 127.0.0.1 origin
// is accepted by the same-origin check.
//
// Returns the dial URL (ws://<host>:<port>); the underlying server is
// registered with t.Cleanup so callers don't need to track it.
func startEchoWS(t *testing.T, fn func(ctx context.Context, conn *websocket.Conn)) string {
	t.Helper()
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck // best-effort
		fn(r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	return "ws://" + strings.TrimPrefix(server.URL, "http://")
}

// TestCoderWSAdapter_RoundTrip verifies the adapter ferries one frame
// of bytes through ReadFrame / WriteFrame in both directions. The
// server-side handler echoes back the first message it reads; the
// adapter under test wraps the client side.
func TestCoderWSAdapter_RoundTrip(t *testing.T) {
	t.Parallel()

	dialURL := startEchoWS(t, func(ctx context.Context, c *websocket.Conn) {
		_, msg, err := c.Read(ctx)
		if err != nil {
			t.Errorf("server read: %v", err)
			return
		}
		if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
			t.Errorf("server write: %v", err)
		}
		_ = c.Close(websocket.StatusNormalClosure, "done")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, dialURL, nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	adapter := newCoderWSAdapter(conn, "1.2.3.4:5678")

	require.NoError(t, adapter.WriteFrame(ctx, []byte(`hello`)))

	got, err := adapter.ReadFrame(ctx)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}

// TestCoderWSAdapter_RemoteAddr verifies the adapter surfaces the
// stored remote-addr verbatim. The handler stores it at construction
// because *websocket.Conn does not expose the underlying TCP peer
// after upgrade.
func TestCoderWSAdapter_RemoteAddr(t *testing.T) {
	t.Parallel()

	adapter := newCoderWSAdapter(nil, "10.0.0.1:42")
	assert.Equal(t, "10.0.0.1:42", adapter.RemoteAddr())
}

// TestCoderWSAdapter_CloseCodes verifies CloseReason values are mapped
// onto the canonical websocket.StatusCode set. The adapter does not
// invent new codes; rtapi.CloseReason is already numeric and matches
// the RFC 6455 / 4xxx custom codes.
func TestCoderWSAdapter_CloseCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		reason rtapi.CloseReason
		want   websocket.StatusCode
	}{
		{"normal", rtapi.CloseNormal, websocket.StatusNormalClosure},
		{"going-away", rtapi.CloseGoingAway, websocket.StatusGoingAway},
		{"protocol-err", rtapi.CloseProtocolErr, websocket.StatusProtocolError},
		{"invalid-data", rtapi.CloseInvalidData, websocket.StatusUnsupportedData},
		{"policy", rtapi.ClosePolicyViol, websocket.StatusPolicyViolation},
		{"unauthorized", rtapi.CloseUnauthorized, websocket.StatusCode(4401)},
		{"rate-limited", rtapi.CloseRateLimited, websocket.StatusCode(4429)},
	}
	for _, tc := range cases {
		got := closeReasonToStatus(tc.reason)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

// TestCoderWSAdapter_CloseClient drives a real Accept/Dial loop and
// asserts the client-side adapter Close fires the WS close frame so
// the server reads close. Goleak-clean: every spawned goroutine exits
// once the conn closes.
func TestCoderWSAdapter_CloseClient(t *testing.T) {
	t.Parallel()

	serverDone := make(chan websocket.StatusCode, 1)
	dialURL := startEchoWS(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, err := c.Read(ctx)
		require.Error(t, err)
		serverDone <- websocket.CloseStatus(err)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, dialURL, nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	adapter := newCoderWSAdapter(conn, "x")
	require.NoError(t, adapter.Close(rtapi.CloseGoingAway, "bye"))

	// context.WithTimeout instead of time.After (carry-forward rule 5
	// keeps select-loops free of time.After; a one-shot is OK but
	// ctx-derived timeouts are the project house style).
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer waitCancel()
	select {
	case got := <-serverDone:
		assert.Equal(t, websocket.StatusGoingAway, got)
	case <-waitCtx.Done():
		t.Fatal("server did not observe close in time")
	}
}

// TestCoderWSAdapter_NilConn_PerOp verifies every per-op method returns
// a clear error when the underlying conn is nil. This is defence-in-
// depth against a wiring bug where the handler forgets to wrap the
// upgraded conn before calling lifecycle methods.
func TestCoderWSAdapter_NilConn_PerOp(t *testing.T) {
	t.Parallel()
	a := newCoderWSAdapter(nil, "0.0.0.0:0")

	_, err := a.ReadFrame(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil websocket.Conn")

	err = a.WriteFrame(context.Background(), []byte("hi"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil websocket.Conn")

	err = a.Close(rtapi.CloseNormal, "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil websocket.Conn")
}

// TestCloseReasonToStatus_UnknownFallsBack verifies an unknown
// CloseReason maps to InternalError so a future addition to the
// CloseReason set surfaces as an explicit "internal" status rather
// than a silently-truncated zero.
func TestCloseReasonToStatus_UnknownFallsBack(t *testing.T) {
	t.Parallel()
	got := closeReasonToStatus(rtapi.CloseReason(9999))
	assert.Equal(t, websocket.StatusInternalError, got)
}

// TestCoderWSAdapter_ReadFrameOnClosedConn verifies a Read against a
// closed conn returns a non-nil error so the realtime Connection's
// reader exits cleanly.
func TestCoderWSAdapter_ReadFrameOnClosedConn(t *testing.T) {
	t.Parallel()

	dialURL := startEchoWS(t, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Close(websocket.StatusNormalClosure, "no traffic")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, dialURL, nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	adapter := newCoderWSAdapter(conn, "x")
	_, err = adapter.ReadFrame(ctx)
	require.Error(t, err)
	// Either a graceful close OR a generic "use of closed connection"
	// is acceptable; the contract is "non-nil error so the reader
	// goroutine in service.Connection unwinds via Close".
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadFrame returned a ctx error %v; expected a websocket close error", err)
	}
}

//go:build smoke

package smoke

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// OperatorWS wraps a coder/websocket connection scoped to one
// /api/operator/ws session. The wrapper owns the *websocket.Conn and
// exposes the two methods scenario 3 needs: ReadEvent (one JSON frame
// with timeout) and Close (graceful close + nil-safe).
//
// We keep the wrapper deliberately minimal — write is not exposed
// because the dialer's WS handler does not consume client frames as
// data (only control frames drive ping/pong). Future scenarios that
// need to send data add a Write* method here.
//
// closeOnce gates Close so callers that invoke it from a defer AND
// from a test failure path do not race on the underlying conn — the
// second call is a no-op (returns the cached err from the first call).
type OperatorWS struct {
	conn      *websocket.Conn
	closeOnce sync.Once
	closeErr  error
}

// DialOperator opens a WebSocket connection to /api/operator/ws on
// addr, attaching the supplied JWT via the ?token= query parameter.
// The dialer's WS handler self-authenticates against the validator
// using this query parameter (see internal/dialer/transport/http/ws.go
// :80-95) — the production browser path, not an Authorization header,
// because browsers cannot easily set headers on a WebSocket handshake.
//
// addr is the cmd/api HTTP listener address ("127.0.0.1:NNNN"); the
// WebSocket URL is built as ws://<addr>/api/operator/ws?token=<jwt>.
//
// The dialer mounts NO subprotocol (verified — see internal/dialer/
// transport/http/ws.go::websocket which builds AcceptOptions with
// OriginPatterns only). We pass nil DialOptions so the client does not
// request a subprotocol either, matching the canonical browser flow.
//
// Returns:
//   - *OperatorWS on a successful upgrade (HTTP 101 Switching Protocols).
//   - non-nil error on dial failure, non-101 response, or context done.
//
// The caller MUST eventually invoke (*OperatorWS).Close to release the
// underlying *websocket.Conn — leaking it keeps a goroutine alive for
// the duration of the process.
//
// ctx applies to the dial AND the readEvent loop; a cancelled ctx
// surfaces as a non-nil error from both. Most callers pass t.Context()
// (auto-cancelled at test end) plus a context.WithTimeout for the
// per-call budget.
func DialOperator(ctx context.Context, t *testing.T, addr, jwt string) (*OperatorWS, error) {
	t.Helper()

	wsURL := buildOperatorWSURL(addr, jwt)
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	// resp is the underlying *http.Response. coder/websocket reads the
	// upgrade response internally before handing the conn back, but
	// resp.Body is exposed for callers that want to surface status. We
	// close it explicitly so bodyclose stays happy and a future
	// transport-error path (resp non-nil, err non-nil) does not leak
	// the underlying connection.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		// Surface the response status code in the error message when
		// available — a future regression where the server returns 401
		// instead of upgrading to 101 then surfaces as a recognisable
		// "expected 101, got 401" rather than a vague "dial failed".
		// resp may be nil on a TCP-level failure (ECONNREFUSED) —
		// guard accordingly.
		if resp != nil {
			return nil, fmt.Errorf("smoke: ws dial %s: status=%d: %w", wsURL, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("smoke: ws dial %s: %w", wsURL, err)
	}
	return &OperatorWS{conn: conn}, nil
}

// buildOperatorWSURL renders the canonical ws:// URL the WS handler
// expects. Extracted so DialOperator stays linear; verified against
// internal/dialer/transport/http/routes.go:147 (mount point is
// /api/operator/ws under the parent /api group).
//
// jwt is URL-escaped to defend against any future test that passes a
// token containing reserved characters (`+`, `/`, `=` from base64).
// The dialer's handler reads the raw token from c.Query("token") which
// already decodes the value, so escaping here is reversible and safe.
func buildOperatorWSURL(addr, jwt string) string {
	u := url.URL{
		Scheme:   "ws",
		Host:     addr,
		Path:     "/api/operator/ws",
		RawQuery: "token=" + url.QueryEscape(jwt),
	}
	return u.String()
}

// ReadEvent reads exactly one JSON frame from the WebSocket and decodes
// it into a generic map[string]any. timeout caps the wait; a frame that
// does not arrive in time surfaces as context.DeadlineExceeded wrapped
// with the call site for diagnostic clarity.
//
// We decode into map[string]any (rather than a typed DTO) because each
// scenario asserts on different fields (snapshot vs state-change vs
// keep-alive) and a typed shape would force a schema PR every time the
// dialer's wire format grew a field. The map shape is what
// internal/realtime/api/events.go and dialer Snapshot serialise to via
// json.Marshal.
//
// Returns:
//   - map[string]any on a successful frame decode.
//   - error wrapping wsjson.Read's failure (timeout, conn close,
//     malformed JSON).
func (w *OperatorWS) ReadEvent(ctx context.Context, timeout time.Duration) (map[string]any, error) {
	if w == nil || w.conn == nil {
		return nil, errors.New("smoke: OperatorWS.ReadEvent on nil/closed connection")
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var msg map[string]any
	if err := wsjson.Read(rctx, w.conn, &msg); err != nil {
		return nil, fmt.Errorf("smoke: ws read: %w", err)
	}
	return msg, nil
}

// Close issues a normal-closure WebSocket frame and returns. Idempotent
// + nil-safe so deferred cleanups don't double-close. Errors from the
// first call are returned to the caller for telemetry; subsequent calls
// return the cached error from the first close. Plan-21-style t.Cleanup
// invocations typically discard the return value.
//
// Order matters: we Close the conn FIRST, cache the err, and only THEN
// observably "null" the receiver state via sync.Once. The previous
// revision nulled `conn` BEFORE the close call returned, so a caller
// that wanted to retry on a transient close error had no conn left to
// retry against. sync.Once also makes the close-twice case explicit.
//
// The "graceful close" frame mirrors the dialer's own
// closeGracefully(websocket.StatusNormalClosure, "") in
// internal/dialer/transport/http/ws.go — both sides agree the
// session ended cleanly, no hung goroutine on either side.
func (w *OperatorWS) Close() error {
	if w == nil || w.conn == nil {
		return nil
	}
	w.closeOnce.Do(func() {
		w.closeErr = w.conn.Close(websocket.StatusNormalClosure, "smoke test done")
	})
	return w.closeErr
}

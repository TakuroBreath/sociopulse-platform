package http

import (
	"context"
	"fmt"

	"github.com/coder/websocket"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// coderWSAdapter wraps a *coder/websocket.Conn so it satisfies the
// realtime layer's rtapi.WSConn surface. The realtime Connection is
// the SOLE caller of this type: production wiring builds one in the
// /ws gin handler immediately after websocket.Accept and passes it to
// service.NewConnection.
//
// Goroutine safety: the underlying *websocket.Conn serialises Read
// from a single goroutine and Write from a single goroutine — the
// realtime Connection's runReader / runWriter / runPinger split is
// designed for that exact contract.
//
// The adapter is intentionally thin — no buffering, no marshalling —
// because the realtime Connection layer already owns those concerns.
type coderWSAdapter struct {
	conn       *websocket.Conn
	remoteAddr string
}

// Compile-time guarantee the adapter satisfies the rtapi.WSConn
// contract. Plan 09/10 carry-forward.
var _ rtapi.WSConn = (*coderWSAdapter)(nil)

// newCoderWSAdapter wraps a *websocket.Conn alongside the upgrade-time
// remote address (typically c.Request.RemoteAddr from the gin
// context). The adapter caches the remote so the realtime Connection
// can log it without re-acquiring the mutex on the wrapped conn.
//
// A nil conn is permitted at construction time so RemoteAddr can be
// asserted in unit tests; ReadFrame / WriteFrame on a nil conn will
// fail at the call-site with a clear panic that pinpoints the wiring
// bug.
func newCoderWSAdapter(conn *websocket.Conn, remoteAddr string) *coderWSAdapter {
	return &coderWSAdapter{conn: conn, remoteAddr: remoteAddr}
}

// ReadFrame reads one message off the wire and returns its payload.
// The realtime Connection's reader unmarshals the bytes into a Frame.
//
// We only handle MessageText — the wire protocol is JSON-encoded
// frames and a peer that sends MessageBinary is misbehaving. We
// surface that as an error so the reader exits cleanly via Close.
func (a *coderWSAdapter) ReadFrame(ctx context.Context) ([]byte, error) {
	if a.conn == nil {
		return nil, fmt.Errorf("realtime/transport/http: ReadFrame: nil websocket.Conn")
	}
	typ, data, err := a.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, fmt.Errorf("realtime/transport/http: unexpected message type %v (want text)", typ)
	}
	return data, nil
}

// WriteFrame writes one MessageText frame. The realtime Connection's
// writer is the only legitimate caller; a per-frame WriteTimeout is
// applied at that layer via context.WithTimeout, so this method does
// not impose its own deadline.
func (a *coderWSAdapter) WriteFrame(ctx context.Context, data []byte) error {
	if a.conn == nil {
		return fmt.Errorf("realtime/transport/http: WriteFrame: nil websocket.Conn")
	}
	return a.conn.Write(ctx, websocket.MessageText, data)
}

// Close finishes the WebSocket close handshake. The reason string is
// truncated by coder/websocket to fit the close-frame budget.
//
// Idempotent: a second Close on an already-closed conn returns nil
// because the realtime Connection layer's closeOnce guarantees the
// adapter sees at most one Close call per lifecycle, but defending
// against the double-close path keeps the contract tight.
func (a *coderWSAdapter) Close(reason rtapi.CloseReason, text string) error {
	if a.conn == nil {
		return fmt.Errorf("realtime/transport/http: Close: nil websocket.Conn")
	}
	return a.conn.Close(closeReasonToStatus(reason), text)
}

// RemoteAddr returns the remote-addr captured at construction time.
// Used by the realtime Connection's structured-log fields.
func (a *coderWSAdapter) RemoteAddr() string { return a.remoteAddr }

// closeReasonToStatus maps an rtapi.CloseReason onto the
// coder/websocket StatusCode set. The realtime layer's CloseReason
// values mirror RFC 6455 (1xxx) plus the project's custom 4xxx codes;
// the conversion is therefore a literal cast for known values.
//
// Unknown reasons fall back to InternalError (1011) so a future
// CloseReason addition that forgets to extend this switch surfaces as
// an explicit "internal" rather than a silently-truncated zero.
func closeReasonToStatus(r rtapi.CloseReason) websocket.StatusCode {
	switch r {
	case rtapi.CloseNormal:
		return websocket.StatusNormalClosure
	case rtapi.CloseGoingAway:
		return websocket.StatusGoingAway
	case rtapi.CloseProtocolErr:
		return websocket.StatusProtocolError
	case rtapi.CloseInvalidData:
		return websocket.StatusUnsupportedData
	case rtapi.ClosePolicyViol:
		return websocket.StatusPolicyViolation
	case rtapi.CloseUnauthorized:
		return websocket.StatusCode(4401)
	case rtapi.CloseRateLimited:
		return websocket.StatusCode(4429)
	default:
		return websocket.StatusInternalError
	}
}

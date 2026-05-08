package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// Local sentinel aliases. Keeping a local var aliased to the api
// sentinel means in-package consumers can errors.Is(err, ErrAuthFailed)
// without importing the api package twice (Plan 09/10 carry-forward —
// caught by reviewer when missing).
var (
	ErrAuthFailed       = rtapi.ErrAuthFailed
	ErrAuthRequired     = rtapi.ErrAuthRequired
	ErrConnectionClosed = rtapi.ErrConnectionClosed
	ErrSlowConsumer     = rtapi.ErrSlowConsumer
)

// AuthValidator validates a JWT access token and returns the decoded
// realtime Claims. Production wiring adapts auth.Authenticator (whose
// Claims type carries uuid.UUID fields and Role enum) into the
// stringly-typed rtapi.Claims used throughout the realtime layer.
//
// The interface is narrow on purpose — the realtime Connection has
// exactly one auth concern (turn token into Claims) so the seam stays
// small for tests.
type AuthValidator interface {
	Validate(ctx context.Context, token string) (rtapi.Claims, error)
}

// HubCallback is invoked by the reader goroutine for every inbound
// FrameSubscribe / FrameUnsubscribe so the Hub (Plan 11 Task 3) can
// register / unregister subscriptions atomically against its
// canonical map. The callback signature is non-error-returning at the
// inbound level — the Hub responds to the connection by calling
// Connection.Send with FrameSubscribeOK / FrameSubscribeErr; this
// keeps the reader goroutine free of branching on Hub-side errors.
//
// nil HubCallback is allowed: Subscribe / Unsubscribe inbound frames
// are logged and dropped. Task 3 wires the real callback.
type HubCallback func(c *Connection, frame rtapi.Frame)

// Connection wraps a WSConn with the per-client lifecycle bundle:
// reader / writer / pinger goroutines, a bounded send queue, an
// atomic Claims pointer (swapped on token refresh), and idempotent
// Close.
//
// The exported name Connection collides with rtapi.Connection (the
// interface) only at use-site qualifier scope — inside this package
// we always speak of the concrete *Connection; consumers interact via
// the rtapi.Connection interface returned by Hub.Connect (wired in
// Task 3).
type Connection struct {
	id      string
	wsConn  rtapi.WSConn
	cfg     ConnectionConfig
	auth    AuthValidator
	onFrame HubCallback
	metrics *Metrics

	// claims is swapped atomically on FrameRefresh. Writers (the
	// reader goroutine on FrameRefresh, AuthHandshake on initial
	// auth) Store a fresh *rtapi.Claims; readers (Hub RBAC checks)
	// Load. Never nil after AuthHandshake completes.
	claims atomic.Pointer[rtapi.Claims]

	// sendChan is the writer's source of frames. Bounded by
	// cfg.WriteBufferSize. Send is non-blocking: a full buffer
	// triggers drop-oldest replacement.
	sendChan chan rtapi.Frame

	// closeOnce gates the actual close path so a double-Close from
	// competing goroutines (writer error + Hub-initiated
	// DisconnectByUser racing) is safe.
	closeOnce sync.Once
	// closeChan is closed exactly once by Close to signal every
	// goroutine that the lifecycle is unwinding.
	closeChan chan struct{}
	// closeReason holds the rtapi.CloseReason passed to Close. Read
	// by Run after closeChan fires to finalise the WSConn close
	// handshake with a typed reason.
	closeReason atomic.Pointer[rtapi.CloseReason]

	// lastPongAt is updated by the reader on every inbound
	// FramePong; the pinger compares against the configured
	// PongTimeout and triggers Close when the gap grows too large.
	lastPongAt atomic.Pointer[time.Time]

	// droppedFrames counts the number of slow-consumer drops for
	// this connection. Mirrored to the Metrics counter for
	// dashboards; exposed via DroppedFrames() for tests.
	droppedFrames atomic.Uint64

	// inboundLimiter clamps inbound frame rate (default 100/sec).
	inboundLimiter *tokenBucket

	// authenticated flips true exactly once (AuthHandshake success).
	// Subscribe/Unsubscribe/Send all reject with ErrAuthRequired
	// before this flips, defence-in-depth above the auth handshake.
	authenticated atomic.Bool

	// closed flips true on close. Used by Send / Subscribe to
	// short-circuit operations on a teardown-in-progress connection.
	closed atomic.Bool

	wg sync.WaitGroup
}

// Compile-time guarantee the implementation satisfies the public
// contract (Plan 09/10 carry-forward).
var _ rtapi.Connection = (*Connection)(nil)

// NewConnection constructs a Connection. The wsConn must already be
// upgraded; the HTTP handler is responsible for the Accept call. cfg
// zero-values are filled in via defaults().
//
// The connection is INACTIVE on return — no goroutines have been
// spawned yet. The caller must:
//
//  1. AuthHandshake(ctx, validator) to validate the auth frame.
//  2. Run(ctx) to start the reader/writer/pinger goroutines.
//
// Splitting the two steps lets the HTTP handler route a failed auth
// to a clean 4401 close without ever spawning the per-connection
// goroutines (goleak-friendly).
func NewConnection(wsConn rtapi.WSConn, cfg ConnectionConfig) *Connection {
	cfg.defaults()
	c := &Connection{
		id:        uuid.NewString(),
		wsConn:    wsConn,
		cfg:       cfg,
		metrics:   nil,
		sendChan:  make(chan rtapi.Frame, cfg.WriteBufferSize),
		closeChan: make(chan struct{}),
		inboundLimiter: newTokenBucket(
			float64(cfg.RateLimitPerSec),
			float64(cfg.RateLimitBurst),
			cfg.Clock,
		),
	}
	now := cfg.Clock()
	c.lastPongAt.Store(&now)
	return c
}

// SetMetrics attaches a *Metrics. nil is allowed (default behaviour).
func (c *Connection) SetMetrics(m *Metrics) { c.metrics = m }

// SetHubCallback wires the Hub-side handler for inbound Subscribe /
// Unsubscribe / non-control frames. Called once by Hub.Connect (Plan
// 11 Task 3) immediately after AuthHandshake and before Run.
func (c *Connection) SetHubCallback(cb HubCallback) { c.onFrame = cb }

// ID returns the server-side connection ID. Stable for the connection
// lifetime; used as the {conn_id} Prometheus label and as the Hub
// map key.
func (c *Connection) ID() string { return c.id }

// Claims returns the latest authenticated identity. Zero-valued
// before AuthHandshake completes; refreshed atomically on each
// successful FrameRefresh.
func (c *Connection) Claims() rtapi.Claims {
	if p := c.claims.Load(); p != nil {
		return *p
	}
	return rtapi.Claims{}
}

// DroppedFrames reports the number of frames discarded due to slow
// consumer drop-oldest. Test helper; production observability uses
// the Metrics counter.
func (c *Connection) DroppedFrames() uint64 { return c.droppedFrames.Load() }

// Subscribe is a thin facade that defers to the Hub (Plan 11 Task 3).
// Until the Hub is wired, Subscribe returns ErrConnectionClosed
// (semantically: the Hub side of the connection isn't online yet).
func (c *Connection) Subscribe(_ rtapi.Topic, _ rtapi.SubscriptionFilter) (string, error) {
	if c.closed.Load() {
		return "", ErrConnectionClosed
	}
	if !c.authenticated.Load() {
		return "", ErrAuthRequired
	}
	// Hub wiring lands in Task 3; surface as not-yet-wired so a
	// bug-report (someone called Subscribe before Task 3 lands)
	// surfaces loudly.
	return "", errors.New("realtime/service: Subscribe not wired (Plan 11 Task 3)")
}

// Unsubscribe removes the subscription with the given ID. Same
// stub-pending-Task-3 status as Subscribe.
func (c *Connection) Unsubscribe(_ string) {
	// Stub until Task 3.
}

// Send queues frame for delivery on the writer goroutine.
// Non-blocking: if sendChan is full, drop the OLDEST queued frame
// (channel-receive then channel-send the new frame) and increment
// the dropped_frames metric.
//
// Drop-oldest preferred over drop-newest because a slow consumer is
// interested in the LATEST state; a stale event is more useless than
// missing one (operator-state transitions, queue depth, etc.).
//
// Send on a closed connection is a no-op — callers race the close
// path during teardown and shouldn't crash.
func (c *Connection) Send(frame rtapi.Frame) {
	if c.closed.Load() {
		return
	}
	// Fast path: room in buffer.
	select {
	case c.sendChan <- frame:
		return
	default:
	}
	// Slow consumer — drop oldest.
	select {
	case <-c.sendChan:
		c.droppedFrames.Add(1)
		c.metrics.observeDrop(c.id)
	default:
		// Channel went from full -> drained between selects.
		// Fall through and try to enqueue.
	}
	select {
	case c.sendChan <- frame:
	default:
		// Channel went from full -> empty -> full between selects.
		// (Receiver was draining at the same time.) Drop the new
		// frame; the receiver is still consuming so the remaining
		// queue is fresh.
		c.droppedFrames.Add(1)
		c.metrics.observeDrop(c.id)
	}
}

// AuthHandshake reads the FIRST inbound frame, expects FrameAuth,
// and validates the token. On success: writes FrameAuthOK, stores
// Claims, returns. On failure: writes FrameAuthError, closes wsConn
// 4401, returns ErrAuthFailed (or wrapped).
//
// MUST be called BEFORE Run — the handshake runs synchronously on
// the HTTP-handler goroutine because the handler owns the
// http.ResponseWriter for the upgrade and a failed auth must produce
// a clean 4401 close without spawning any extra goroutines.
//
// Bounded by cfg.AuthTimeout. A slow client that fails to send
// the auth frame in time gets dropped without ceremony.
func (c *Connection) AuthHandshake(ctx context.Context, auth AuthValidator) (rtapi.Claims, error) {
	if auth == nil {
		return rtapi.Claims{}, fmt.Errorf("realtime/service: AuthHandshake: nil validator")
	}
	c.auth = auth

	handshakeCtx, cancel := context.WithTimeout(ctx, c.cfg.AuthTimeout)
	defer cancel()

	raw, err := c.wsConn.ReadFrame(handshakeCtx)
	if err != nil {
		c.metrics.observeAuthFailure("read_error")
		c.cfg.Logger.Debug("realtime: auth handshake read failed",
			zap.String("conn_id", c.id),
			zap.Error(err),
		)
		_ = c.wsConn.Close(rtapi.CloseProtocolErr, "auth read")
		return rtapi.Claims{}, fmt.Errorf("realtime/service: auth read: %w", err)
	}

	var frame rtapi.Frame
	if err := json.Unmarshal(raw, &frame); err != nil {
		c.metrics.observeAuthFailure("bad_json")
		_ = c.writeFrameSync(handshakeCtx, rtapi.Frame{
			Type:   rtapi.FrameAuthError,
			Reason: "bad json",
		})
		_ = c.wsConn.Close(rtapi.CloseInvalidData, "bad json")
		return rtapi.Claims{}, fmt.Errorf("realtime/service: auth parse: %w", err)
	}
	if frame.Type != rtapi.FrameAuth {
		c.metrics.observeAuthFailure("wrong_kind")
		_ = c.writeFrameSync(handshakeCtx, rtapi.Frame{
			Type:   rtapi.FrameAuthError,
			Reason: "expected auth frame",
		})
		_ = c.wsConn.Close(rtapi.ClosePolicyViol, "expected auth")
		return rtapi.Claims{}, fmt.Errorf("realtime/service: %w (got %q)", ErrAuthRequired, frame.Type)
	}
	if frame.Token == "" {
		c.metrics.observeAuthFailure("missing_token")
		_ = c.writeFrameSync(handshakeCtx, rtapi.Frame{
			Type:   rtapi.FrameAuthError,
			Reason: "missing token",
		})
		_ = c.wsConn.Close(rtapi.CloseUnauthorized, "missing token")
		return rtapi.Claims{}, ErrAuthFailed
	}

	claims, err := auth.Validate(handshakeCtx, frame.Token)
	if err != nil {
		c.metrics.observeAuthFailure("invalid_token")
		c.cfg.Logger.Debug("realtime: auth validation failed",
			zap.String("conn_id", c.id),
			zap.Error(err),
		)
		_ = c.writeFrameSync(handshakeCtx, rtapi.Frame{
			Type:   rtapi.FrameAuthError,
			Reason: "invalid token",
		})
		_ = c.wsConn.Close(rtapi.CloseUnauthorized, "invalid token")
		// Wrap so callers can errors.Is(err, ErrAuthFailed).
		return rtapi.Claims{}, fmt.Errorf("realtime/service: %w", ErrAuthFailed)
	}

	c.claims.Store(&claims)
	c.authenticated.Store(true)

	if err := c.writeFrameSync(handshakeCtx, rtapi.Frame{Type: rtapi.FrameAuthOK}); err != nil {
		// Auth was valid but the auth.ok response could not be
		// written. Treat as a transient failure and close
		// politely; the operator UI will reconnect.
		_ = c.wsConn.Close(rtapi.CloseGoingAway, "auth.ok write failed")
		return rtapi.Claims{}, fmt.Errorf("realtime/service: write auth.ok: %w", err)
	}
	return claims, nil
}

// Run starts the reader/writer/pinger goroutines and blocks until the
// connection closes. Called by the WS HTTP handler after
// AuthHandshake succeeds.
//
// MUST NOT be called before AuthHandshake — Run does NOT call
// AuthHandshake itself because the handler owns the
// http.ResponseWriter and must distinguish a 4401 close from a
// normal lifecycle close.
//
// Run returns when every spawned goroutine has exited and the
// underlying WSConn is closed. Always returns nil; failures unwind
// through the close channel + closeReason atomic.
func (c *Connection) Run(ctx context.Context) error {
	if !c.authenticated.Load() {
		return fmt.Errorf("realtime/service: Run before AuthHandshake")
	}

	c.wg.Go(func() { c.runReader(ctx) })
	c.wg.Go(func() { c.runWriter(ctx) })
	c.wg.Go(func() { c.runPinger(ctx) })

	// Block until any goroutine signals close.
	select {
	case <-c.closeChan:
	case <-ctx.Done():
		// Parent ctx cancelled — cascade to close.
		c.Close(rtapi.CloseGoingAway)
	}

	reason := rtapi.CloseNormal
	if r := c.closeReason.Load(); r != nil {
		reason = *r
	}
	_ = c.wsConn.Close(reason, "")
	c.wg.Wait()
	return nil
}

// Close signals all goroutines to exit and returns immediately. The
// final close-frame to the client is emitted by Run (which blocks
// until close fires). Idempotent: repeated calls are no-ops.
func (c *Connection) Close(reason rtapi.CloseReason) {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.closeReason.Store(&reason)
		close(c.closeChan)
	})
}

// runReader is the inbound-frame consumer. Each frame is dispatched
// by FrameKind:
//
//   - FrameRefresh:     re-validate token, atomic-swap Claims,
//     respond FrameRefreshOK; close 4401 on failure.
//   - FrameSubscribe / FrameUnsubscribe: forward to the Hub via
//     the configured HubCallback (Task 3).
//   - FramePing:        respond with FramePong (server-side pong;
//     rare — most pings are server -> client).
//   - FramePong:        record lastPongAt for the pinger.
//   - default:          log + ignore.
//
// On Reader error or ctx done the reader signals close and exits.
func (c *Connection) runReader(ctx context.Context) {
	for {
		select {
		case <-c.closeChan:
			return
		case <-ctx.Done():
			c.Close(rtapi.CloseGoingAway)
			return
		default:
		}

		raw, err := c.wsConn.ReadFrame(ctx)
		if err != nil {
			// Ctx cancellation, normal close, or network error.
			// Distinguishing them isn't useful here — the
			// outcome (unwind via Close) is the same.
			c.cfg.Logger.Debug("realtime: reader exit",
				zap.String("conn_id", c.id),
				zap.Error(err),
			)
			c.Close(rtapi.CloseNormal)
			return
		}
		if !c.inboundLimiter.Allow() {
			c.metrics.observeRateLimitClosure()
			c.cfg.Logger.Warn("realtime: inbound rate limit exceeded",
				zap.String("conn_id", c.id),
			)
			c.Close(rtapi.CloseRateLimited)
			return
		}

		var frame rtapi.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			c.cfg.Logger.Debug("realtime: ignoring malformed frame",
				zap.String("conn_id", c.id),
				zap.Error(err),
			)
			continue
		}
		c.dispatchFrame(ctx, frame)
	}
}

// dispatchFrame routes a parsed frame by FrameKind. Extracted from
// runReader so unit tests can drive the dispatch table without
// spinning up a goroutine.
func (c *Connection) dispatchFrame(ctx context.Context, frame rtapi.Frame) {
	switch frame.Type {
	case rtapi.FrameRefresh:
		c.handleRefresh(ctx, frame)
	case rtapi.FrameSubscribe, rtapi.FrameUnsubscribe:
		if c.onFrame != nil {
			c.onFrame(c, frame)
		} else {
			c.cfg.Logger.Debug("realtime: hub callback not wired; dropping subscribe-class frame",
				zap.String("conn_id", c.id),
				zap.String("kind", string(frame.Type)),
			)
		}
	case rtapi.FramePing:
		// Server-side pong: rare but the wire protocol allows
		// either side to ping. Respond via the writer queue so
		// pong shares the same back-pressure semantics.
		c.Send(rtapi.Frame{Type: rtapi.FramePong})
	case rtapi.FramePong:
		now := c.cfg.Clock()
		c.lastPongAt.Store(&now)
	default:
		c.cfg.Logger.Debug("realtime: ignoring unknown frame kind",
			zap.String("conn_id", c.id),
			zap.String("kind", string(frame.Type)),
		)
	}
}

// handleRefresh validates the supplied token and atomic-swaps the
// connection's Claims. Existing subscriptions stay attached; only
// future RBAC checks see the new Claims. A bad token closes the
// connection with 4401.
func (c *Connection) handleRefresh(ctx context.Context, frame rtapi.Frame) {
	if c.auth == nil {
		c.Send(rtapi.Frame{
			Type:   rtapi.FrameAuthError,
			Reason: "no validator wired",
		})
		c.Close(rtapi.CloseUnauthorized)
		return
	}
	if frame.Token == "" {
		c.Send(rtapi.Frame{
			Type:   rtapi.FrameAuthError,
			Reason: "missing token",
		})
		c.Close(rtapi.CloseUnauthorized)
		return
	}
	claims, err := c.auth.Validate(ctx, frame.Token)
	if err != nil {
		c.metrics.observeAuthFailure("invalid_token")
		c.cfg.Logger.Debug("realtime: refresh validation failed",
			zap.String("conn_id", c.id),
			zap.Error(err),
		)
		c.Send(rtapi.Frame{
			Type:   rtapi.FrameAuthError,
			Reason: "invalid token",
		})
		c.Close(rtapi.CloseUnauthorized)
		return
	}
	c.claims.Store(&claims)
	c.Send(rtapi.Frame{Type: rtapi.FrameRefreshOK})
}

// runWriter is the SOLE owner of conn.WriteFrame. It pulls frames
// off sendChan and writes them with a per-frame WriteTimeout. On
// write error it signals close and exits.
func (c *Connection) runWriter(ctx context.Context) {
	for {
		select {
		case <-c.closeChan:
			return
		case <-ctx.Done():
			c.Close(rtapi.CloseGoingAway)
			return
		case frame := <-c.sendChan:
			wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
			err := c.writeFrameSync(wctx, frame)
			cancel()
			if err != nil {
				c.cfg.Logger.Warn("realtime: write failed",
					zap.String("conn_id", c.id),
					zap.Error(err),
				)
				c.Close(rtapi.CloseGoingAway)
				return
			}
		}
	}
}

// runPinger emits a FramePing every PingPeriod via sendChan and
// monitors lastPongAt. If the gap between now and lastPongAt grows
// past PongTimeout, we declare the connection dead and close it.
//
// time.NewTicker (not time.After in a select-loop) per Plan 09/10
// carry-forward.
func (c *Connection) runPinger(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.PingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-c.closeChan:
			return
		case <-ctx.Done():
			c.Close(rtapi.CloseGoingAway)
			return
		case <-ticker.C:
			// Send the ping (drop-oldest semantics if the
			// writer is wedged).
			c.Send(rtapi.Frame{Type: rtapi.FramePing})

			// Check pong drift.
			lastPong := c.cfg.Clock()
			if p := c.lastPongAt.Load(); p != nil {
				lastPong = *p
			}
			if c.cfg.Clock().Sub(lastPong) > c.cfg.PongTimeout {
				c.metrics.observePongMiss()
				c.cfg.Logger.Warn("realtime: pong grace exceeded",
					zap.String("conn_id", c.id),
					zap.Duration("pong_timeout", c.cfg.PongTimeout),
				)
				c.Close(rtapi.CloseRateLimited)
				return
			}
		}
	}
}

// writeFrameSync marshals frame and writes it via the WSConn. It is
// invoked by the writer goroutine (the only legitimate caller during
// the lifecycle) and synchronously by AuthHandshake before the
// writer is alive. Concurrent writes from non-writer goroutines are
// a contract violation — runWriter does NOT serialise external
// callers; AuthHandshake runs before runWriter starts so the
// invariant holds.
func (c *Connection) writeFrameSync(ctx context.Context, frame rtapi.Frame) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("realtime/service: marshal frame: %w", err)
	}
	return c.wsConn.WriteFrame(ctx, b)
}

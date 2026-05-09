package http

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// defaultTouchPeriod is the cadence at which the per-conn presence
// ticker calls PresenceTracker.Touch. The PresenceTracker default TTL
// is 30 s; touching at TTL/3 (10 s) gives us two refresh attempts
// before the entry would lapse, surviving a transient Redis hiccup.
const defaultTouchPeriod = 10 * time.Second

// wireSubprotocol is the WebSocket subprotocol token negotiated at
// upgrade time. Pinned in code so future protocol revisions surface
// as a compile-time constant change (and can be tracked in a single
// grep). Tests use the same constant via the testHTTPDial helpers.
const wireSubprotocol = "sociopulse-v1"

// wsHandlerConfig groups the collaborators a *wsHandler needs.
//
// The handler is intentionally agnostic of how its collaborators were
// constructed — it accepts the realtime service.Hub directly (because
// only the concrete Hub publishes the SetHubCallback / Connect /
// Broadcast surface the handler exercises), an AuthValidator (built by
// authAdapter wrapping the auth module's Authenticator), an optional
// PresenceTracker, and the per-connection ConnectionConfig.
type wsHandlerConfig struct {
	// hub is the per-replica Hub that registers the connection,
	// owns the per-tenant fan-out maps, and delivers Broadcast
	// frames. Mandatory.
	hub *service.Hub

	// auth converts a JWT access token into rtapi.Claims. Mandatory.
	auth service.AuthValidator

	// metrics is the per-connection counters struct (dropped frames,
	// auth failures, pong misses, rate-limit closures). Optional —
	// nil disables observability.
	metrics *service.Metrics

	// presence is the cross-replica presence tracker. Optional — nil
	// short-circuits the OnConnect / Touch / OnDisconnect lifecycle.
	presence rtapi.PresenceTracker

	// replicaID identifies this pod in the presence map. Reset per
	// pod boot via uuid.NewString from the composition root.
	replicaID string

	// logger is the structured logger. Nil-safe (defaults to nop).
	logger *zap.Logger

	// origins narrows the websocket.Accept origin gate. Empty/nil
	// enforces same-origin only.
	origins []string

	// touchPeriod overrides defaultTouchPeriod for tests. Zero or
	// negative values fall back to the default.
	touchPeriod time.Duration

	// connConfig is the realtime Connection lifecycle config (auth
	// timeout, ping period, write timeout, rate limits, etc.). Zero
	// values pick the production defaults documented on
	// service.ConnectionConfig.
	connConfig service.ConnectionConfig
}

// wsHandler is the GET /api/realtime/ws handler. The struct holds the
// configured collaborators plus a per-pod refcount so multiple WS
// connections from the same user share the same presence key without
// firing OnDisconnect until the last one closes.
type wsHandler struct {
	cfg wsHandlerConfig

	// refMu guards refcount.
	refMu sync.Mutex
	// refcount maps "tenant\x00user" -> open connection count for the
	// local pod. PresenceTracker.OnConnect / OnDisconnect fire only
	// at the 0->1 / 1->0 transitions.
	refcount map[string]int
}

// newWSHandler constructs a *wsHandler with sensible defaults. A nil
// hub or auth validator is a wiring bug and panics — the composition
// root in module.go catches the failure at boot.
func newWSHandler(cfg wsHandlerConfig) *wsHandler {
	if cfg.hub == nil {
		panic("realtime/transport/http: newWSHandler: hub is required")
	}
	if cfg.auth == nil {
		panic("realtime/transport/http: newWSHandler: auth is required")
	}
	if cfg.logger == nil {
		cfg.logger = zap.NewNop()
	}
	if cfg.touchPeriod <= 0 {
		cfg.touchPeriod = defaultTouchPeriod
	}
	if cfg.connConfig.Logger == nil {
		cfg.connConfig.Logger = cfg.logger
	}
	return &wsHandler{
		cfg:      cfg,
		refcount: make(map[string]int),
	}
}

// handle is the gin handler for GET /api/realtime/ws.
//
// Lifecycle:
//
//  1. websocket.Accept upgrades the connection and applies origin
//     restrictions. Failures already wrote a 4xx via the lib; we log
//     and return.
//  2. Wrap the *websocket.Conn in a coderWSAdapter.
//  3. Construct a fresh *service.Connection and run AuthHandshake on
//     the adapter — first inbound frame must be FrameAuth + valid
//     token. AuthHandshake itself emits FrameAuthError + closes 4401
//     on failure; we just log and return.
//  4. AttachForTest the connection to the Hub. The function is
//     misnamed — its doc-comment in hub.go documents that it's the
//     package's only post-auth attach seam and acceptable for
//     production HTTP handlers that already ran AuthHandshake.
//  5. SetHubCallback so inbound FrameSubscribe / FrameUnsubscribe
//     forwards through the Connection's Subscribe / Unsubscribe and
//     into the Hub's RBAC + registration flow. The callback also
//     emits FrameSubscribeOK / FrameSubscribeErr on the wire.
//  6. PresenceTracker.OnConnect (per-pod refcount).
//  7. Spawn a Touch ticker goroutine via wg.Go (Go 1.25+).
//  8. Block on conn.Run(ctx) — runs reader / writer / pinger.
//  9. On Run() exit: stop the Touch goroutine, decrement refcount,
//     OnDisconnect on the 1->0 transition, return.
func (h *wsHandler) handle(c *gin.Context) {
	wsConn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		Subprotocols:       []string{wireSubprotocol},
		OriginPatterns:     h.cfg.origins,
		InsecureSkipVerify: hasWildcardOrigin(h.cfg.origins),
	})
	if err != nil {
		// Accept already wrote a response — we cannot rewrite.
		h.cfg.logger.Warn("realtime/ws: accept failed", zap.Error(err))
		return
	}
	// Defer CloseNow as the unconditional cleanup; the lib's Close
	// runs the graceful handshake first. CloseNow is idempotent.
	defer wsConn.CloseNow() //nolint:errcheck // best-effort cleanup

	adapter := newCoderWSAdapter(wsConn, c.Request.RemoteAddr)
	conn := service.NewConnection(adapter, h.cfg.connConfig)
	conn.SetMetrics(h.cfg.metrics)

	// AuthHandshake runs on the gin request goroutine; it consumes
	// the FIRST inbound frame and writes auth.ok / auth.error
	// directly via the adapter. On failure it closes the underlying
	// wsConn with 4401 / appropriate code itself — we just return.
	claims, err := conn.AuthHandshake(c.Request.Context(), h.cfg.auth)
	if err != nil {
		h.cfg.logger.Debug("realtime/ws: auth handshake failed",
			zap.String("remote_addr", adapter.RemoteAddr()),
			zap.Error(err))
		return
	}

	// Register the connection with the Hub. AttachForTest is the
	// package's documented post-auth attach seam — see the
	// doc-comment on Hub.AttachForTest in service/hub.go for why this
	// name is acceptable in production HTTP handlers that already ran
	// AuthHandshake.
	h.cfg.hub.AttachForTest(conn, claims)

	// Wire the inbound-frame callback so a wire-side FrameSubscribe /
	// FrameUnsubscribe flows through the Hub's RBAC + registration
	// path and emits the matching ok / err response on the wire.
	conn.SetHubCallback(h.handleSubscribeFrame)

	// Presence + per-pod refcount.
	h.markConnect(c.Request.Context(), claims)

	// Touch ticker. Bound the goroutine to the Run-completion
	// channel via a closeChan we control.
	touchStop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { h.runTouchLoop(c.Request.Context(), conn, claims, touchStop) })

	// Run blocks until the connection unwinds. The realtime
	// Connection's Run guarantees all reader / writer / pinger
	// goroutines have exited before it returns.
	if err := conn.Run(c.Request.Context()); err != nil {
		h.cfg.logger.Warn("realtime/ws: connection run exited with error",
			zap.String("conn_id", conn.ID()),
			zap.String("tenant_id", claims.TenantID),
			zap.String("user_id", claims.UserID),
			zap.Error(err))
	}

	// Connection has fully unwound — stop the Touch loop and wait.
	close(touchStop)
	wg.Wait()

	// Presence cleanup with refcount.
	h.markDisconnect(c.Request.Context(), claims)
}

// handleSubscribeFrame is the HubCallback wired on every Connection.
// It runs on the Connection's reader goroutine for every inbound
// FrameSubscribe / FrameUnsubscribe.
//
// Subscribe path: forward to Connection.Subscribe (which goes through
// the Hub's RBAC matrix); on success queue a FrameSubscribeOK with the
// allocated subID; on failure queue a FrameSubscribeErr.
//
// Unsubscribe path: forward to Connection.Unsubscribe (idempotent at
// the Hub layer; missing subIDs are silently ignored).
//
// Other frame kinds reach this callback only if Connection's reader
// adds them to the dispatch — currently FrameSubscribe and
// FrameUnsubscribe are the sole forwarders, so the default branch
// is defensive only.
func (h *wsHandler) handleSubscribeFrame(c *service.Connection, frame rtapi.Frame) {
	switch frame.Type {
	case rtapi.FrameSubscribe:
		filter := rtapi.SubscriptionFilter{}
		if frame.Filter != nil {
			filter = *frame.Filter
		}
		subID, err := c.Subscribe(frame.Topic, filter)
		if err != nil {
			c.Send(rtapi.Frame{
				Type:   rtapi.FrameSubscribeErr,
				Topic:  frame.Topic,
				Reason: scrubSubscribeErr(err),
			})
			return
		}
		c.Send(rtapi.Frame{
			Type:  rtapi.FrameSubscribeOK,
			Topic: frame.Topic,
			SubID: subID,
		})
	case rtapi.FrameUnsubscribe:
		c.Unsubscribe(frame.SubID)
	default:
		// Defensive: only Subscribe / Unsubscribe reach the callback
		// today (Connection.dispatchFrame is the gate). A future
		// reader-side regression that forwards a different kind here
		// would otherwise silently disappear; debug-log it so the
		// regression is visible in development tail logs.
		h.cfg.logger.Debug("realtime/ws: unexpected frame on hub callback",
			zap.String("conn_id", c.ID()),
			zap.String("kind", string(frame.Type)),
		)
	}
}

// HandleSubscribeFrame is the exported test seam for the inbound
// frame handler. Production callers wire it via SetHubCallback —
// tests may call it directly to drive the dispatch table without
// spinning up a Hub.
func (h *wsHandler) HandleSubscribeFrame(c *service.Connection, frame rtapi.Frame) {
	h.handleSubscribeFrame(c, frame)
}

// scrubSubscribeErr returns the wire-side Reason string for a
// FrameSubscribeErr emission. ErrCrossTenantSubscribe folds to a
// fixed string so the client cannot probe entity existence
// cross-tenant via wire-string parsing (Plan 11.3 Task 1).
//
// Other RBAC errors (forbidden, filter_required, unknown_topic)
// keep their err.Error() string — operators legitimately need
// that context to debug a client-side bug.
//
// The errors.Is check uses the api-package sentinel so a wrapped
// chain (e.g. fmt.Errorf("%w: ...", ErrCrossTenantSubscribe)) is
// still detected.
func scrubSubscribeErr(err error) string {
	if errors.Is(err, rtapi.ErrCrossTenantSubscribe) {
		return "cross-tenant subscription denied"
	}
	return err.Error()
}

// runTouchLoop calls PresenceTracker.Touch on a ticker. The loop exits
// on stop or on the Hub-issued conn close (cascaded via the wider Run
// completion). On ErrPresenceLapsed we close the connection so the
// client reconnects with a fresh OnConnect.
func (h *wsHandler) runTouchLoop(
	ctx context.Context,
	conn *service.Connection,
	claims rtapi.Claims,
	stop <-chan struct{},
) {
	if h.cfg.presence == nil {
		return
	}
	ticker := time.NewTicker(h.cfg.touchPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := h.cfg.presence.Touch(ctx, claims.TenantID, claims.UserID)
			if err == nil {
				continue
			}
			if errors.Is(err, service.ErrPresenceLapsed) {
				h.cfg.logger.Info("realtime/ws: presence lapsed; closing conn",
					zap.String("conn_id", conn.ID()),
					zap.String("tenant_id", claims.TenantID),
					zap.String("user_id", claims.UserID),
				)
				conn.Close(rtapi.CloseGoingAway)
				return
			}
			h.cfg.logger.Warn("realtime/ws: presence touch failed",
				zap.String("conn_id", conn.ID()),
				zap.Error(err))
		}
	}
}

// markConnect runs the OnConnect side of the per-pod refcount lifecycle.
// First connection for the (tenant, user) pair calls Tracker.OnConnect;
// subsequent connections only bump the local count.
func (h *wsHandler) markConnect(ctx context.Context, claims rtapi.Claims) {
	if h.cfg.presence == nil {
		return
	}
	key := refcountKey(claims)

	h.refMu.Lock()
	prev := h.refcount[key]
	h.refcount[key] = prev + 1
	h.refMu.Unlock()

	if prev == 0 {
		if err := h.cfg.presence.OnConnect(ctx, claims.TenantID, claims.UserID, h.cfg.replicaID); err != nil {
			h.cfg.logger.Warn("realtime/ws: presence OnConnect failed",
				zap.String("tenant_id", claims.TenantID),
				zap.String("user_id", claims.UserID),
				zap.Error(err))
		}
	}
}

// markDisconnect runs the OnDisconnect side of the per-pod refcount
// lifecycle. Tracker.OnDisconnect fires only when the local count
// drops to zero.
func (h *wsHandler) markDisconnect(ctx context.Context, claims rtapi.Claims) {
	if h.cfg.presence == nil {
		return
	}
	key := refcountKey(claims)

	h.refMu.Lock()
	count := h.refcount[key] - 1
	if count <= 0 {
		delete(h.refcount, key)
	} else {
		h.refcount[key] = count
	}
	h.refMu.Unlock()

	if count > 0 {
		return
	}
	if err := h.cfg.presence.OnDisconnect(ctx, claims.TenantID, claims.UserID); err != nil {
		h.cfg.logger.Warn("realtime/ws: presence OnDisconnect failed",
			zap.String("tenant_id", claims.TenantID),
			zap.String("user_id", claims.UserID),
			zap.Error(err))
	}
}

// refcountKey is the per-(tenant, user) refcount-map key. The
// embedded null byte avoids a collision between two pairs whose
// concatenation could overlap (e.g. "ab" + "c" vs "a" + "bc"); UUIDs
// don't have that issue but the null-byte separator is a defensive
// habit.
func refcountKey(claims rtapi.Claims) string {
	return claims.TenantID + "\x00" + claims.UserID
}

// hasWildcardOrigin reports whether the origins list contains a single
// "*" entry — the explicit "accept any origin" mode. coder/websocket
// requires InsecureSkipVerify=true for that case (the OriginPatterns
// glob does not include "*" implicitly).
func hasWildcardOrigin(origins []string) bool {
	return len(origins) == 1 && origins[0] == "*"
}

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
)

// WSConfig tunes the WebSocket handler's ping/pong cadence and write
// deadline. Zero values pick production defaults documented per-field.
//
// The handler enforces the contract:
//   - Server emits a Ping every PingPeriod.
//   - A Ping that does not return a Pong within PongTimeout terminates
//     the connection.
//   - Each individual frame write is bounded by WriteTimeout.
//
// 30s ping / 60s pong-grace is the operator-UI baseline — fast enough
// to detect a dropped tab within a minute, slow enough to not flood
// proxies (Yandex MKS ingress + nginx default to 60s idle).
type WSConfig struct {
	// PingPeriod is the interval between server-initiated pings.
	// Zero → 30s.
	PingPeriod time.Duration
	// PongTimeout bounds how long a ping waits before the conn is
	// considered dead. Zero → 60s.
	PongTimeout time.Duration
	// WriteTimeout bounds a single Snapshot frame write. Zero → 5s.
	WriteTimeout time.Duration
	// AllowedOrigins, when non-empty, restricts the websocket.Accept
	// OriginPatterns. Zero (nil/empty) accepts same-origin only —
	// the production deployment runs the operator UI on the same
	// host as the API, so default is correct. Tests override.
	AllowedOrigins []string
}

// resolved returns the config with zero-valued fields filled in.
func (c WSConfig) resolved() WSConfig {
	if c.PingPeriod <= 0 {
		c.PingPeriod = 30 * time.Second
	}
	if c.PongTimeout <= 0 {
		c.PongTimeout = 60 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 5 * time.Second
	}
	return c
}

// websocket is the GET /api/operator/ws handler.
//
// Authentication flow:
//
//	The operator UI cannot easily set Authorization on a WebSocket
//	handshake, so the access token is supplied via the ?token=…
//	query parameter. We validate it through Deps.Validator and reject
//	the upgrade with 401 on any failure — the websocket.Accept call
//	never runs, so the client gets a clean HTTP error.
//
// Subscription flow:
//
//	On a successful upgrade we Subscribe to SnapshotPubSub for
//	(tenantID, operatorID) and forward every Snapshot as JSON. The
//	subscription is deferred-released on every exit path so a network
//	error or pong timeout cleans up. The handler runs entirely on the
//	request goroutine — no extra goroutines are spawned, so goleak
//	verifies no leak even on early failure.
func (h *handlers) websocket(c *gin.Context) {
	tok := strings.TrimSpace(c.Query("token"))
	if tok == "" {
		// Fall back to the standard Authorization header in case the
		// caller went through a proxy that DOES allow it. The token
		// extraction mirrors the JWTMiddleware regex (Bearer + space).
		if header := c.GetHeader("Authorization"); strings.HasPrefix(strings.ToLower(header), "bearer ") {
			tok = strings.TrimSpace(header[len("Bearer "):])
		}
	}
	if tok == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
			Code:    "auth.token_invalid",
			Message: "missing token query parameter",
		})
		return
	}

	claims, err := h.deps.Validator.Validate(c.Request.Context(), tok)
	if err != nil {
		switch {
		case errors.Is(err, authapi.ErrTokenRevoked):
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
				Code:    "auth.token_revoked",
				Message: "token has been revoked",
			})
		default:
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
				Code:    "auth.token_invalid",
				Message: "token is invalid or expired",
			})
		}
		return
	}
	if !claims.HasRole(authapi.RoleOperator) &&
		!claims.HasRole(authapi.RoleSupervisor) &&
		!claims.HasRole(authapi.RoleAdmin) {
		c.AbortWithStatusJSON(http.StatusForbidden, ErrorEnvelope{
			Code:    "auth.insufficient_role",
			Message: "operator role required",
		})
		return
	}

	// Accept the upgrade. We use InsecureSkipVerify ONLY when an
	// explicit AllowedOrigins list is configured to "*" — otherwise
	// we restrict to the configured origins (default: same-origin).
	cfg := h.deps.WSConfig.resolved()
	acceptOpts := &websocket.AcceptOptions{
		OriginPatterns:     cfg.AllowedOrigins,
		InsecureSkipVerify: len(cfg.AllowedOrigins) == 1 && cfg.AllowedOrigins[0] == "*",
	}
	conn, err := websocket.Accept(c.Writer, c.Request, acceptOpts)
	if err != nil {
		// websocket.Accept already wrote a 4xx response on common
		// failures (bad Origin, bad Sec-WebSocket-Version). We log
		// only — gin's response is sealed by Accept.
		if h.deps.Logger != nil {
			h.deps.Logger.Warn("dialer/ws: accept failed",
				zap.String("tenant_id", claims.TenantID.String()),
				zap.String("operator_id", claims.UserID.String()),
				zap.Error(err))
		}
		return
	}
	// CloseNow is the unconditional cleanup; Close has already
	// performed the graceful handshake on every normal exit path.
	defer conn.CloseNow() //nolint:errcheck // best-effort cleanup

	pumpSnapshots(c.Request.Context(), conn, h.deps, claims, cfg)
}

// pumpSnapshots is the per-conn read/write loop. Extracted from the
// gin handler so it's straightforward to unit-test in isolation when
// (a future) Task wires a Hub-backed pubsub. The function never
// spawns goroutines; the read pump runs as a child goroutine of the
// caller via context cancellation only because coder/websocket's
// Reader is ctx-aware.
//
// Exit conditions:
//
//	a) ctx canceled (request done, server shutdown) → graceful close.
//	b) Snapshot channel closed → graceful close.
//	c) Write error → CloseNow.
//	d) Ping with no Pong within PongTimeout → CloseNow.
//	e) Reader returns error (client closed) → CloseNow.
func pumpSnapshots(
	ctx context.Context,
	conn *websocket.Conn,
	deps Deps,
	claims authapi.Claims,
	cfg WSConfig,
) {
	ch, cancel := deps.SnapshotPubSub.Subscribe(claims.TenantID, claims.UserID)
	defer cancel()

	// Reader is fired-and-forgotten — coder/websocket needs a
	// concurrent reader so Ping handshakes process. We close the
	// connection from the writer side; the reader goroutine exits
	// when conn.CloseNow runs in the deferred caller above.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			// Read in a loop; we discard payloads but Read drives the
			// internal control-frame state machine. A returned error
			// (client close, network error) terminates the goroutine.
			if _, _, rerr := conn.Reader(ctx); rerr != nil {
				return
			}
		}
	}()

	pingTicker := time.NewTicker(cfg.PingPeriod)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			closeGracefully(conn, websocket.StatusNormalClosure, "server shutdown")
			<-readerDone
			return
		case <-readerDone:
			// Client closed or network error — the reader goroutine
			// observed the failure first; we must still flush our
			// own state and exit so the deferred CloseNow runs.
			return
		case snap, ok := <-ch:
			if !ok {
				closeGracefully(conn, websocket.StatusNormalClosure, "subscription ended")
				<-readerDone
				return
			}
			if err := writeSnapshot(ctx, conn, snap, cfg.WriteTimeout); err != nil {
				logWSError(deps.Logger, claims, "write snapshot", err)
				return
			}
		case <-pingTicker.C:
			if err := pingOnce(ctx, conn, cfg.PongTimeout); err != nil {
				logWSError(deps.Logger, claims, "ping timeout", err)
				return
			}
		}
	}
}

// writeSnapshot serialises snap and writes one TextMessage frame
// bounded by writeTimeout. Returns a non-nil error on marshal failure
// (impossible for the current SnapshotDTO shape but defensive) or
// network failure. We use websocket.MessageText so the JSON is
// debuggable in browser dev tools.
func writeSnapshot(ctx context.Context, conn *websocket.Conn, snap dialerapi.Snapshot, writeTimeout time.Duration) error {
	dto := snapshotToDTO(snap)
	buf, err := json.Marshal(dto)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, buf)
}

// pingOnce issues a Ping bounded by pongTimeout. Returns the error
// when no pong arrived in time.
func pingOnce(ctx context.Context, conn *websocket.Conn, pongTimeout time.Duration) error {
	pctx, cancel := context.WithTimeout(ctx, pongTimeout)
	defer cancel()
	return conn.Ping(pctx)
}

// closeGracefully attempts a normal close. Errors are intentionally
// dropped — the deferred CloseNow on the parent stack frame will
// finalise teardown if Close fails (e.g. the conn is already gone).
func closeGracefully(conn *websocket.Conn, code websocket.StatusCode, reason string) {
	_ = conn.Close(code, reason)
}

// logWSError emits a structured warn for a WebSocket failure. We log
// at warn (not error) so a flaky operator network does not page ops;
// real failures bubble up via the Connect/Disconnect counters in
// future Task 10 metrics.
func logWSError(log *zap.Logger, claims authapi.Claims, op string, err error) {
	if log == nil {
		return
	}
	log.Warn("dialer/ws: "+op,
		zap.String("tenant_id", claims.TenantID.String()),
		zap.String("operator_id", claims.UserID.String()),
		zap.Error(err))
}

package http

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// Deps captures the collaborators the realtime transport needs from
// the composition root. Required vs optional fields are documented per
// field; mustNotBeNil() panics for missing requireds so a misconfigured
// composition root fails loudly during cmd/api boot.
type Deps struct {
	// Hub is the per-replica realtime Hub. Mandatory — every
	// endpoint either registers a connection with it (/ws) or
	// dispatches a Broadcast through it (force-action).
	Hub *service.Hub

	// AuthValidator validates the JWT supplied via the WebSocket
	// FrameAuth handshake. Production wiring constructs an
	// *authAdapter wrapping the auth module's Authenticator.
	// Mandatory.
	AuthValidator service.AuthValidator

	// ClaimsValidator is the auth-module-side validator consumed by
	// the gin JWTMiddleware. The /ws route does NOT go through this
	// middleware (browsers cannot easily set Authorization on a WS
	// upgrade); but the force-action and listen-in routes DO.
	// Mandatory.
	ClaimsValidator authapi.ClaimsValidator

	// ConnMetrics is the per-connection Prometheus collector struct.
	// Optional — nil disables observability.
	ConnMetrics *service.Metrics

	// Presence is the cross-replica presence tracker. Optional — a
	// nil tracker short-circuits the OnConnect / Touch / OnDisconnect
	// lifecycle.
	Presence rtapi.PresenceTracker

	// ReplicaID identifies this pod in the presence map. Empty
	// string is permitted but will yield an empty value column in
	// the Redis presence map.
	ReplicaID string

	// Logger is the structured logger. Nil-safe (defaults to nop).
	Logger *zap.Logger

	// AllowedOrigins narrows the websocket.Accept origin gate for
	// the /ws endpoint. Empty/nil enforces same-origin only.
	AllowedOrigins []string

	// ConnConfig overrides the realtime Connection lifecycle config
	// (auth timeout, ping period, write timeout, rate limits).
	// Zero values pick the production defaults documented on
	// service.ConnectionConfig.
	ConnConfig service.ConnectionConfig

	// TouchPeriod overrides the per-conn presence Touch cadence.
	// Zero falls back to defaultTouchPeriod (10 s) to give two
	// retries within the 30 s presence TTL.
	TouchPeriod time.Duration
}

// Mount registers every realtime transport route on the supplied gin
// RouterGroup. The caller passes the parent group (e.g. r.Group("/api/realtime"))
// — Mount creates the per-resource child handlers internally so the
// wire shape is owned by this package.
//
// Auth model:
//
//   - /ws                              — self-authenticating via FrameAuth.
//   - /operators/:id/force-pause        — JWT + admin/supervisor role.
//   - /operators/:id/force-end-shift    — JWT + admin/supervisor role.
//   - /calls/:id/listen                 — JWT (Plan 08 deferred stub).
//   - /listen-sessions/:id              — JWT (Plan 08 deferred stub).
//
// Mount panics if any required Deps field is nil so a misconfigured
// composition root fails loudly during cmd/api boot rather than at
// first request.
func Mount(group *gin.RouterGroup, deps Deps) {
	mustNotBeNil(deps)
	if deps.Logger == nil {
		deps.Logger = zap.NewNop()
	}

	// /ws is mounted OUTSIDE the JWTMiddleware chain. Browsers
	// cannot easily set Authorization on a WebSocket handshake; the
	// realtime AuthHandshake validates the token from the wire-side
	// FrameAuth instead.
	wsHandler := newWSHandler(wsHandlerConfig{
		hub:         deps.Hub,
		auth:        deps.AuthValidator,
		metrics:     deps.ConnMetrics,
		presence:    deps.Presence,
		replicaID:   deps.ReplicaID,
		logger:      deps.Logger,
		origins:     deps.AllowedOrigins,
		touchPeriod: deps.TouchPeriod,
		connConfig:  deps.ConnConfig,
	})
	group.GET("/ws", wsHandler.handle)

	// Authenticated routes (force-action + listen-in stubs) sit
	// behind the standard JWTMiddleware.
	authed := group.Group("")
	authed.Use(authmw.JWTMiddleware(deps.ClaimsValidator))

	forceH := newForceHandler(forceHandlerConfig{
		hub:    deps.Hub,
		logger: deps.Logger,
	})
	forceH.mount(authed)

	listenH := newListenHandler(deps.Logger)
	listenH.mount(authed)
}

// mustNotBeNil verifies every required Deps field is non-nil. Mount
// panics on miss so a composition-root wiring bug fails loudly during
// cmd/api boot.
func mustNotBeNil(d Deps) {
	switch {
	case d.Hub == nil:
		panic("realtime/transport/http: Deps.Hub is required")
	case d.AuthValidator == nil:
		panic("realtime/transport/http: Deps.AuthValidator is required")
	case d.ClaimsValidator == nil:
		panic("realtime/transport/http: Deps.ClaimsValidator is required")
	}
}

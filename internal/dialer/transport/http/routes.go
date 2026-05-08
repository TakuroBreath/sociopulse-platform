package http

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// Deps captures the collaborators that the dialer transport needs.
// Logger may be nil in tests — render paths gate on nil. The FSM and
// Router are mandatory; the optional checkers (Hours, Capacity) are
// reserved for future enrichment hooks (e.g. surfacing "next allowed
// at" in StartShift) and are not currently consulted by the handlers.
type Deps struct {
	FSM      dialerapi.OperatorFSM
	Router   dialerapi.Router
	Queue    dialerapi.CallQueue
	Hours    dialerapi.WorkingHoursChecker
	Capacity dialerapi.LineCapacityTracker

	Validator authapi.ClaimsValidator
	RBAC      authapi.RBACChecker
	Logger    *zap.Logger

	// SnapshotPubSub is the per-(tenant, operator) fan-out source for
	// /api/operator/ws. The FSM (or its outbox subscriber) publishes
	// Snapshots; the WS handler forwards them as JSON. Required when
	// the WS route is mounted; tests pass a fake.
	SnapshotPubSub SnapshotPubSub

	// WSConfig optionally overrides the WebSocket ping/pong / write
	// timeouts. Zero value picks production defaults — see ws.go.
	WSConfig WSConfig

	// RefreshPresence is the optional adapter the heartbeat middleware
	// invokes on every authenticated operator request. Composition root
	// (internal/dialer/module.go) closes over fsm.RefreshPresence + the
	// shared *redis.Client. Nil disables the middleware — useful for
	// Redis-less test setups; the heartbeat watchdog still runs in
	// production so missing this wiring degrades gracefully (operators
	// who only ever hit HTTP without the WS keep-alive can be force-
	// paused after one watchdog sweep, which is the pre-Plan 11.1
	// behaviour).
	RefreshPresence RefreshFn

	// Metrics is the optional observability surface for the transport.
	// Nil disables every observation; production wires
	// RegisterMetrics(reg) and passes the result here.
	Metrics *Metrics
}

// SnapshotPubSub is the tiny pub/sub seam the WS handler uses to
// receive Snapshot updates for the connected operator. The Subscribe
// channel is buffered by the implementation; a slow consumer drops
// snapshots silently rather than blocking the FSM commit path.
type SnapshotPubSub interface {
	// Subscribe registers a receiver for snapshots scoped to the
	// (tenantID, operatorID) pair. The returned cancel function is
	// idempotent and MUST be invoked exactly once per Subscribe so the
	// pub/sub releases the subscriber slot. The channel is closed by
	// the implementation only after cancel completes.
	Subscribe(tenantID, operatorID uuid.UUID) (<-chan dialerapi.Snapshot, func())
}

// Mount registers every dialer transport route on the supplied gin
// RouterGroup. The caller passes the parent (e.g. the /api group);
// Mount creates the per-resource child groups internally so the
// wire shape is owned by this package.
//
// Auth model:
//
//	all routes require a valid JWT (JWTMiddleware on the parent group).
//	/sessions/*, /calls/*/status, /calls/*/hangup — operator role.
//	/operator/ws — operator role (token via query parameter).
//	/operator/verify/* — supervisor role.
//	/operator/:id/force — admin role.
//
// Mount panics if any required Deps field is nil so a misconfigured
// composition root fails loudly during cmd/api boot rather than at
// first request. The optional fields (Queue, Hours, Capacity) are
// permitted to be nil; the WS-related SnapshotPubSub is required.
func Mount(group *gin.RouterGroup, deps Deps) {
	mustNotBeNil(deps)
	h := &handlers{deps: deps}

	// Every dialer route requires authentication.
	authed := group.Group("")
	authed.Use(authmw.JWTMiddleware(deps.Validator))

	// Heartbeat presence refresh — applied AFTER JWTMiddleware so claims
	// are populated. Mounted at the authed-group level (rather than on
	// the operator-only sessions/calls subgroups) so any future
	// operator-driven endpoint inherits the refresh without rewiring;
	// supervisor / admin claims pass through cheaply since the
	// watchdog's SCAN pattern (op:*:user:*) only matches operator
	// hashes — refreshing a supervisor's presence:<t>:user:<sid> key
	// is a no-op against the watchdog's evict path. nil RefreshPresence
	// yields a no-op middleware (factory short-circuits at construction).
	authed.Use(refreshPresenceMiddleware(deps.RefreshPresence, deps.Metrics, deps.Logger))

	// Operator self-service — sessions / calls.
	sessions := authed.Group("/sessions")
	sessions.Use(requireRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin))
	sessions.POST("/start", h.startShift)
	sessions.POST("/end", h.endShift)
	sessions.POST("/pause", h.goPause)
	sessions.POST("/resume", h.resume)
	sessions.GET("/me", h.getMe)

	calls := authed.Group("/calls")
	calls.Use(requireRole(authapi.RoleOperator, authapi.RoleSupervisor, authapi.RoleAdmin))
	calls.POST("/:id/status", h.submitStatus)
	calls.POST("/:id/hangup", h.hangup)

	// Operator real-time channel — mounted OUTSIDE the JWTMiddleware
	// chain because browsers cannot easily set Authorization on a
	// WebSocket handshake. The WS handler self-authenticates against
	// Deps.Validator using the ?token= query parameter (with an
	// Authorization-header fallback for non-browser clients) and
	// enforces the operator-role gate in-line.
	group.GET("/operator/ws", h.websocket)

	// Supervisor / admin escapes — JWT-protected.
	operator := authed.Group("/operator")
	verify := operator.Group("/verify")
	verify.Use(requireRole(authapi.RoleSupervisor, authapi.RoleAdmin))
	verify.POST("/start", h.goVerify)
	verify.POST("/done", h.verifyDone)

	// Admin force escape — :id is the target operator.
	operator.POST("/:id/force", requireRole(authapi.RoleAdmin), h.force)
}

// mustNotBeNil verifies every required collaborator. We panic so a
// composition-root misconfiguration fails loudly during cmd/api boot.
func mustNotBeNil(d Deps) {
	switch {
	case d.FSM == nil:
		panic("dialer/transport/http: FSM is required")
	case d.Router == nil:
		panic("dialer/transport/http: Router is required")
	case d.Validator == nil:
		panic("dialer/transport/http: Validator is required")
	case d.RBAC == nil:
		panic("dialer/transport/http: RBAC is required")
	case d.SnapshotPubSub == nil:
		panic("dialer/transport/http: SnapshotPubSub is required")
	}
}

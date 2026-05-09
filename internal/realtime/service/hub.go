package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/google/uuid"
	"go.uber.org/zap"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// Hub is the per-replica WebSocket fan-out core. Each cmd/api pod
// runs exactly one *Hub. NATS dispatcher (Plan 11 Task 4) calls
// Hub.Broadcast as events arrive on tenant.<t>.> subjects. WS
// handlers call Hub.Connect after AuthHandshake succeeds.
//
// Goroutine safety: all maps are protected by hub.mu (RWMutex). Read
// paths (Broadcast lookup, Stats) take RLock; mutate paths (Connect /
// Subscribe / Unsubscribe / Disconnect) take Lock. The connection's
// Send is called while holding RLock in Broadcast — Send is
// non-blocking by contract (drop-oldest on a full telemetryCh,
// close-connection on a full criticalCh per Plan 11.2), so a slow
// consumer doesn't extend lock-hold beyond a memory write +
// channel-send.
//
// Lifecycle:
//
//   - NewHub: empty registries, no goroutines spawned.
//   - Connect / AttachForTest: register conn in byTenant + flat,
//     wire subscribeFn / unsubFn / onClose callbacks on the
//     Connection, increment HubMetrics.Connections.
//   - Connection.Subscribe → Hub.subscribeForConn: RBAC check, then
//     write to subs + bySubConn under Lock. Returns the new subID.
//   - Connection.Unsubscribe → Hub.unsubscribeForConn: drop sub from
//     subs + bySubConn under Lock.
//   - Connection.Close (any path) → onCloseCallback: drop conn from
//     byTenant + flat + clean up any lingering subs.
//   - Hub.DisconnectByUser: iterate flat for matching (tenant, user),
//     close each. The onClose callback handles registry cleanup, so
//     DisconnectByUser does not call delete() directly — it just
//     invokes Connection.Close, which fires the cleanup hook.
//   - Hub.Shutdown: walk flat, Close each, idempotent. Hub itself
//     spawns no goroutines, so there's no wg.Wait().
//
// Plan 09/10 carry-forward:
//   - var _ rtapi.Hub = (*Hub)(nil) compile-time check.
//   - *zap.Logger typed; nil-safe (defaults to NewNop).
//   - No init()-time MustRegister; HubMetrics is constructed via
//     RegisterHubMetrics by the composition root.
//   - No goroutines, so no goleak concern at the Hub layer (every
//     Connection-spawned goroutine is the Connection's
//     responsibility).
type Hub struct {
	mu      sync.RWMutex
	log     *zap.Logger
	rbac    *TopicRBAC
	metrics *HubMetrics

	// byTenant -> tenantID -> connID -> *Connection. Primary lookup
	// for Broadcast (filter.TenantID is required, so we narrow the
	// scan to one tenant immediately).
	byTenant map[string]map[string]*Connection

	// flat -> connID -> *Connection. Secondary lookup used by
	// DisconnectByUser (which has tenantID + userID but no connID)
	// and admin debug surfaces.
	flat map[string]*Connection

	// subs -> subID -> *subRecord. Canonical subscription map keyed
	// by the server-side subID. Lookups for Unsubscribe + lifecycle
	// cleanup walk this map.
	subs map[string]*subRecord

	// bySubConn -> connID -> set of subIDs. Per-connection back-link
	// so a Connection.Close can clean up every dangling sub in O(s)
	// where s is the per-conn subscription count.
	bySubConn map[string]map[string]struct{}

	closeOnce sync.Once
}

// subRecord is the canonical projection of a subscription stored
// on the Hub side. The Subscription DTO in rtapi/dto.go is the
// public-facing shape; subRecord is the internal storage form.
type subRecord struct {
	id     string
	connID string
	topic  rtapi.Topic
	filter rtapi.SubscriptionFilter
}

// Compile-time guarantee the implementation satisfies the public
// contract. Plan 09/10 carry-forward.
var _ rtapi.Hub = (*Hub)(nil)

// NewHub constructs an empty Hub. Caller injects the RBAC matrix and
// the Hub-level metrics; both are nil-safe.
//
// log nil → zap.NewNop. rbac nil is forbidden — a Hub without an
// RBAC matrix would silently allow every subscription, which is a
// security regression we'd never want a forgotten-DI bug to cause.
// Panic at construction time so the failure surfaces at boot, not at
// first Subscribe call.
func NewHub(log *zap.Logger, metrics *HubMetrics, rbac *TopicRBAC) *Hub {
	if log == nil {
		log = zap.NewNop()
	}
	if rbac == nil {
		panic("realtime/service.NewHub: rbac must be non-nil")
	}
	return &Hub{
		log:       log,
		rbac:      rbac,
		metrics:   metrics,
		byTenant:  make(map[string]map[string]*Connection),
		flat:      make(map[string]*Connection),
		subs:      make(map[string]*subRecord),
		bySubConn: make(map[string]map[string]struct{}),
	}
}

// Connect registers a freshly-upgraded WSConn with the Hub. The HTTP
// layer is responsible for the AuthHandshake (or equivalent
// upgrade-time auth); claims must be populated when this returns.
//
// Connect:
//  1. Constructs a *Connection wrapping the WSConn.
//  2. Seeds the connection's claims so RBAC checks see the right
//     identity without a wire-side handshake.
//  3. Registers the connection in byTenant + flat under hub.mu.
//  4. Wires subscribeFn / unsubFn / onClose callbacks so subsequent
//     Connection.{Subscribe,Unsubscribe,Close} flow back into the Hub.
//
// Returns ErrAuthRequired if claims is the zero value
// (TenantID is empty). The HTTP layer should never call Connect with
// zero claims; the guard is defence-in-depth.
func (h *Hub) Connect(_ context.Context, conn rtapi.WSConn, claims rtapi.Claims) (rtapi.Connection, error) {
	if claims.TenantID == "" {
		return nil, ErrAuthRequired
	}

	c := NewConnection(conn, ConnectionConfig{Logger: h.log})
	c.SeedClaims(claims)
	h.attach(c)
	return c, nil
}

// AttachForTest registers a *Connection that the test has already
// constructed (typically with a fakeWSConn) and seeds its claims.
// Tests use this to drive Hub.Subscribe / Broadcast / Disconnect
// flows without bringing a real WSConn online.
//
// AttachForTest is intentionally exported — the alternative (Hub
// type-assertion gymnastics) makes the test code unreadable and
// leaves the Hub's Connect path under-tested. The "ForTest" suffix
// flags it as out-of-band wiring; production code should always go
// through Connect.
func (h *Hub) AttachForTest(c *Connection, claims rtapi.Claims) {
	c.SeedClaims(claims)
	h.attach(c)
}

// attach is the shared registration path for Connect + AttachForTest.
// Holds hub.mu.Lock for the entire registration so a concurrent
// Broadcast can't see a half-registered conn (in flat but not
// byTenant, or vice versa).
func (h *Hub) attach(c *Connection) {
	claims := c.Claims()

	h.mu.Lock()
	if h.byTenant[claims.TenantID] == nil {
		h.byTenant[claims.TenantID] = make(map[string]*Connection)
	}
	h.byTenant[claims.TenantID][c.ID()] = c
	h.flat[c.ID()] = c
	h.mu.Unlock()

	// Wire callbacks AFTER registration so a callback firing from a
	// racing close can find the conn in the maps to unregister.
	c.setSubscribeFn(h.subscribeForConn)
	c.setUnsubscribeFn(h.unsubscribeForConn)
	c.setOnClose(h.onConnClose)

	h.metrics.observeConnect()

	h.log.Debug("realtime: hub registered conn",
		zap.String("conn_id", c.ID()),
		zap.String("tenant_id", claims.TenantID),
		zap.String("user_id", claims.UserID),
	)
}

// subscribeForConn is the Hub-side handler invoked from
// Connection.Subscribe. Validates the RBAC matrix, allocates a
// subID, and atomically inserts the subscription into the canonical
// maps.
//
// The function is a SubscribeFn (matched at construction). Lock
// discipline: the registry-mutation phase (write to subs +
// bySubConn) holds hub.mu.Lock; the RBAC check and uuid allocation
// run lock-free against the immutable matrix and a goroutine-local
// random source.
func (h *Hub) subscribeForConn(c *Connection, topic rtapi.Topic, filter rtapi.SubscriptionFilter) (string, error) {
	if err := h.rbac.Allow(c.Claims(), topic, filter); err != nil {
		h.metrics.observeSubscribeFailure(string(topic), classifyRBACErr(err))
		return "", err
	}

	subID := uuid.NewString()
	rec := &subRecord{
		id:     subID,
		connID: c.ID(),
		topic:  topic,
		filter: filter,
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// If the conn isn't registered, refuse — the contract is
	// Hub.Connect → Subscribe; an unattached conn calling Subscribe
	// is a wiring bug.
	if _, ok := h.flat[c.ID()]; !ok {
		return "", errors.New("realtime/service: subscribe: conn not registered with hub")
	}

	h.subs[subID] = rec
	if h.bySubConn[c.ID()] == nil {
		h.bySubConn[c.ID()] = make(map[string]struct{})
	}
	h.bySubConn[c.ID()][subID] = struct{}{}

	h.metrics.observeSubscribe(string(topic))

	return subID, nil
}

// unsubscribeForConn is the Hub-side handler invoked from
// Connection.Unsubscribe. Idempotent: removing an unknown subID is a
// silent no-op. Removing a subID that belongs to a different conn
// (defence-in-depth — should never happen in production) is also a
// no-op.
func (h *Hub) unsubscribeForConn(c *Connection, subID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeSubLocked(c.ID(), subID)
}

// removeSubLocked drops one subID from subs + bySubConn[connID].
// Must be called with hub.mu held in write mode.
func (h *Hub) removeSubLocked(connID, subID string) {
	rec, ok := h.subs[subID]
	if !ok {
		return
	}
	if rec.connID != connID {
		// Mismatched connID — defensive guard. Log and skip; a
		// real production path would never hit this.
		h.log.Warn("realtime: hub Unsubscribe: subID belongs to other conn",
			zap.String("sub_id", subID),
			zap.String("conn_id", connID),
			zap.String("owner_conn_id", rec.connID),
		)
		return
	}
	delete(h.subs, subID)
	if cs := h.bySubConn[connID]; cs != nil {
		delete(cs, subID)
		if len(cs) == 0 {
			delete(h.bySubConn, connID)
		}
	}
	h.metrics.observeUnsubscribe(string(rec.topic))
}

// onConnClose is the Hub-side cleanup callback wired into every
// registered *Connection. Fires exactly once per connection (gated
// by Connection.closeOnce) when Close is invoked.
//
// Drops the connection from byTenant + flat and removes every
// subscription it owns. Idempotent against double-cleanup
// (DisconnectByUser closing a conn that already triggered close
// from its reader exit path) because the underlying maps tolerate
// missing keys.
func (h *Hub) onConnClose(c *Connection) {
	tenantID := c.Claims().TenantID
	connID := c.ID()

	h.mu.Lock()
	defer h.mu.Unlock()

	if subSet := h.bySubConn[connID]; subSet != nil {
		for subID := range subSet {
			if rec, ok := h.subs[subID]; ok {
				h.metrics.observeUnsubscribe(string(rec.topic))
				delete(h.subs, subID)
			}
		}
		delete(h.bySubConn, connID)
	}

	if conns, ok := h.byTenant[tenantID]; ok {
		delete(conns, connID)
		if len(conns) == 0 {
			delete(h.byTenant, tenantID)
		}
	}
	if _, ok := h.flat[connID]; ok {
		delete(h.flat, connID)
		h.metrics.observeDisconnect()
	}
}

// Broadcast dispatches a payload to every local connection in
// filter.TenantID with a subscription on topic matching the filter.
//
// Filter semantics (mirrors the spec in plan-11-realtime.md):
//
//   - filter.TenantID is REQUIRED. Empty → no broadcast (return 0).
//     This prevents a misconfigured publisher from leaking events
//     across tenants if some upstream forgets to set the field.
//   - filter.UserID — narrow to the matching conn's claims.UserID.
//     Empty UserID = no narrowing.
//   - filter.ProjectID / filter.CallID — must intersect with the
//     SUBSCRIPTION's filter (not the conn's claims). Empty filter
//     fields on either side mean "no constraint on that dimension".
//
// Returns the count of conn.Send invocations (after RBAC + filter
// passes). Drops on the connection's telemetryCh are tracked
// separately via the Connection's dropped_frames metric;
// critical-overflow closures via critical_overflows_total. Hub
// counts dispatches, not deliveries.
//
// Locking: holds RLock for the entire iteration. conn.Send is
// non-blocking by contract, so the lock is held only for the
// duration of a memory write + channel-send. No write paths
// (Subscribe / Disconnect / Connect) can run concurrently — they
// take Lock — so the per-conn subscription-set we read is consistent.
func (h *Hub) Broadcast(_ context.Context, topic rtapi.Topic, payload json.RawMessage, filter rtapi.BroadcastFilter) int {
	if filter.TenantID == "" {
		return 0
	}

	frame := rtapi.Frame{
		Type:    rtapi.FrameEvent,
		Topic:   topic,
		Payload: payload,
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	tenantConns := h.byTenant[filter.TenantID]
	count := 0
	for connID, conn := range tenantConns {
		if filter.UserID != "" && conn.Claims().UserID != filter.UserID {
			continue
		}
		subSet := h.bySubConn[connID]
		if subSet == nil {
			continue
		}
		for subID := range subSet {
			rec := h.subs[subID]
			if rec == nil || rec.topic != topic {
				continue
			}
			if !subFilterMatches(rec.filter, filter) {
				continue
			}
			// Stamp the SubID so the client can correlate the
			// event with its outstanding subscription handle.
			deliver := frame
			deliver.SubID = rec.id
			conn.Send(deliver)
			count++
			break // one frame per connection per broadcast
		}
	}

	h.metrics.observeBroadcast(string(topic), count)

	return count
}

// subFilterMatches reports whether a BroadcastFilter intersects with
// a SubscriptionFilter. The intersection rule:
//
//   - For each named field on BroadcastFilter (ProjectID, CallID),
//     if the broadcast value is empty → no constraint, match.
//   - If the broadcast value is non-empty AND the subscription value
//     is non-empty AND they differ → no match.
//   - Otherwise match (broadcast asserts a value but subscription
//     didn't narrow on it = subscriber accepts everything; or both
//     specify the same value).
//
// Tenant + UserID are checked at the Hub level (Broadcast loop)
// because they live on conn.Claims, not the SubscriptionFilter.
func subFilterMatches(sub rtapi.SubscriptionFilter, b rtapi.BroadcastFilter) bool {
	if b.ProjectID != "" && sub.ProjectID != "" && sub.ProjectID != b.ProjectID {
		return false
	}
	if b.CallID != "" && sub.CallID != "" && sub.CallID != b.CallID {
		return false
	}
	return true
}

// DisconnectByUser closes every connection matching (tenantID, userID).
// Used by auth.SessionRevoker on force-logout-all and by admin
// kick-user tooling. Per-conn cleanup runs through the onClose
// callback; we don't touch the registries directly.
//
// Holds RLock during the scan + collects the matching conns into a
// local slice, then releases the lock and calls Close on each. The
// two-phase pattern prevents a deadlock between hub.mu and the
// onClose callback (which re-acquires hub.mu in write mode).
func (h *Hub) DisconnectByUser(_ context.Context, tenantID, userID string) {
	h.mu.RLock()
	tenantConns := h.byTenant[tenantID]
	victims := make([]*Connection, 0, len(tenantConns))
	for _, c := range tenantConns {
		if c.Claims().UserID == userID {
			victims = append(victims, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range victims {
		c.Close(rtapi.CloseGoingAway)
	}
}

// Stats returns a snapshot of the Hub's runtime state. Used by
// /metrics and the admin /admin/realtime endpoint.
func (h *Hub) Stats() rtapi.HubStats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	stats := rtapi.HubStats{
		Connections:    len(h.flat),
		BySubscription: make(map[rtapi.Topic]int),
	}
	for _, rec := range h.subs {
		stats.BySubscription[rec.topic]++
	}
	return stats
}

// Shutdown closes every registered connection with CloseGoingAway
// (1001). Idempotent: a second call is a no-op gated by closeOnce.
//
// Hub itself spawns no goroutines, so there's no wg.Wait at this
// layer. Connection.Close fires its onClose callback synchronously
// (which removes the conn from registries), but the per-connection
// reader/writer/pinger goroutines spawned by Connection.Run unwind
// asynchronously — Shutdown does NOT block on them. Callers that
// need lifecycle quiescence should hold references to each
// *Connection and observe its Run completion.
func (h *Hub) Shutdown() {
	h.closeOnce.Do(func() {
		h.mu.RLock()
		victims := make([]*Connection, 0, len(h.flat))
		for _, c := range h.flat {
			victims = append(victims, c)
		}
		h.mu.RUnlock()

		for _, c := range victims {
			c.Close(rtapi.CloseGoingAway)
		}
	})
}

// classifyRBACErr maps an RBAC denial to a low-cardinality reason
// label for Subscribe-failure metrics. Unknown errors fall back to
// "internal" so the dashboard catches mis-categorisation.
func classifyRBACErr(err error) string {
	switch {
	case errors.Is(err, ErrTopicForbidden):
		return "forbidden"
	case errors.Is(err, ErrFilterRequired):
		return "filter_required"
	case errors.Is(err, ErrUnknownTopic):
		return "unknown"
	default:
		return "internal"
	}
}

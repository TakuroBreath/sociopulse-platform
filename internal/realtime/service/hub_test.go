// hub_test.go — behaviour tests for *Hub: tenant isolation, broadcast
// filter intersection, subscription RBAC, force disconnect, shutdown.
//
// Test discipline:
//
//   - Every subtest constructs an isolated Hub via newTestHub. The
//     Hub never spawns goroutines itself, so a forgotten Shutdown
//     leaves no leaked work — but t.Cleanup wires Shutdown anyway as
//     defence-in-depth (and to exercise the idempotent path on a
//     well-behaved test).
//   - Connections are constructed via the existing newTestConnection
//     helper (testutil_test.go). The Hub is wired via
//     hub.AttachForTest(conn, claims) — package-private export shim
//     that calls the same registration code the production
//     Hub.Connect path uses, only without driving an actual WSConn
//     handshake.
//   - Tests never call conn.Run, so the writer goroutine never
//     drains the per-conn send queues. Assertions read
//     fakeWSConn.Writes() or DrainSendForTest directly. This keeps
//     tests deterministic and goleak-clean.
package service_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// var _ rtapi.Hub = (*service.Hub)(nil) — compile-time check belongs in
// the implementation file (hub.go); this file only exercises behaviour.

func TestHub_BroadcastIsolatesByTenant(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)

	connA, fakeA := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { connA.Close(rtapi.CloseNormal) })
	hub.AttachForTest(connA, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err := connA.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	connB, fakeB := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { connB.Close(rtapi.CloseNormal) })
	hub.AttachForTest(connB, rtapi.Claims{
		UserID: "u2", TenantID: "tenant-B", Roles: []string{"admin"},
	})
	_, err = connB.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	count := hub.Broadcast(
		context.Background(),
		rtapi.TopicOperatorsState,
		json.RawMessage(`{"x":1}`),
		rtapi.BroadcastFilter{TenantID: "tenant-A"},
	)
	require.Equal(t, 1, count)

	// connA's writer is not running, so the frame sits in the conn's
	// telemetryCh (operators.state classifies as telemetry). We can
	// detect delivery by peeking the conn's drained state via a
	// synchronous Send-equivalent: drain manually or assert via the
	// channel. Use a small goroutine that drains the writer queue.
	require.Eventually(t, func() bool {
		return drainOneFrame(t, connA) != nil
	}, time.Second, 5*time.Millisecond)
	require.Nil(t, drainOneFrame(t, connB), "tenant-B must NOT receive the broadcast")

	// Sanity: the test never wrote anything to the WSConn (no
	// runWriter) so fakeA / fakeB should still have empty Writes.
	require.Empty(t, fakeA.Writes())
	require.Empty(t, fakeB.Writes())
}

func TestHub_BroadcastEmptyTenantIDIsZero(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err := conn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	// No tenant on the BroadcastFilter — must be a no-op.
	got := hub.Broadcast(
		context.Background(),
		rtapi.TopicOperatorsState,
		json.RawMessage(`{}`),
		rtapi.BroadcastFilter{},
	)
	require.Equal(t, 0, got)
}

func TestHub_BroadcastNarrowsByUserID(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)

	c1, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { c1.Close(rtapi.CloseNormal) })
	hub.AttachForTest(c1, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err := c1.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	c2, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { c2.Close(rtapi.CloseNormal) })
	hub.AttachForTest(c2, rtapi.Claims{
		UserID: "u2", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err = c2.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	got := hub.Broadcast(
		context.Background(),
		rtapi.TopicOperatorsState,
		json.RawMessage(`{}`),
		rtapi.BroadcastFilter{TenantID: "tenant-A", UserID: "u1"},
	)
	require.Equal(t, 1, got)
	require.NotNil(t, drainOneFrame(t, c1))
	require.Nil(t, drainOneFrame(t, c2))
}

func TestHub_BroadcastNarrowsByProjectID(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)

	c1, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { c1.Close(rtapi.CloseNormal) })
	hub.AttachForTest(c1, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	// Subscription scoped to project-A.
	_, err := c1.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{ProjectID: "proj-A"})
	require.NoError(t, err)

	c2, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { c2.Close(rtapi.CloseNormal) })
	hub.AttachForTest(c2, rtapi.Claims{
		UserID: "u2", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	// Subscription scoped to project-B.
	_, err = c2.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{ProjectID: "proj-B"})
	require.NoError(t, err)

	// Broadcast scoped to project-A — only c1 receives.
	got := hub.Broadcast(
		context.Background(),
		rtapi.TopicOperatorsState,
		json.RawMessage(`{}`),
		rtapi.BroadcastFilter{TenantID: "tenant-A", ProjectID: "proj-A"},
	)
	require.Equal(t, 1, got)
	require.NotNil(t, drainOneFrame(t, c1))
	require.Nil(t, drainOneFrame(t, c2))
}

func TestHub_BroadcastNarrowsByCallID(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)

	c1, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { c1.Close(rtapi.CloseNormal) })
	hub.AttachForTest(c1, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err := c1.Subscribe(rtapi.TopicCallEvents, rtapi.SubscriptionFilter{CallID: "call-1"})
	require.NoError(t, err)

	c2, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { c2.Close(rtapi.CloseNormal) })
	hub.AttachForTest(c2, rtapi.Claims{
		UserID: "u2", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err = c2.Subscribe(rtapi.TopicCallEvents, rtapi.SubscriptionFilter{CallID: "call-2"})
	require.NoError(t, err)

	got := hub.Broadcast(
		context.Background(),
		rtapi.TopicCallEvents,
		json.RawMessage(`{}`),
		rtapi.BroadcastFilter{TenantID: "tenant-A", CallID: "call-1"},
	)
	require.Equal(t, 1, got)
	require.NotNil(t, drainOneFrame(t, c1))
	require.Nil(t, drainOneFrame(t, c2))
}

func TestHub_BroadcastSkipsConnsWithoutMatchingTopic(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	// Subscribed to TopicOperatorsState — broadcast on TopicDialerQueue.
	_, err := conn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	got := hub.Broadcast(
		context.Background(),
		rtapi.TopicDialerQueue,
		json.RawMessage(`{}`),
		rtapi.BroadcastFilter{TenantID: "tenant-A"},
	)
	require.Equal(t, 0, got)
	require.Nil(t, drainOneFrame(t, conn))
}

func TestHub_SubscribeRejectsViaRBAC(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"operator"},
	})

	// operator may not subscribe to TopicOperatorsState (admin/supervisor only).
	_, err := conn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.ErrorIs(t, err, service.ErrTopicForbidden)
}

func TestHub_SubscribeFilterRequiredEnforced(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})

	_, err := conn.Subscribe(rtapi.TopicCallEvents, rtapi.SubscriptionFilter{})
	require.ErrorIs(t, err, service.ErrFilterRequired)
}

func TestHub_SubscribeUnknownTopic(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})

	_, err := conn.Subscribe(rtapi.Topic("not.a.topic"), rtapi.SubscriptionFilter{})
	require.ErrorIs(t, err, service.ErrUnknownTopic)
}

func TestHub_UnsubscribeStopsBroadcastDelivery(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})

	subID, err := conn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, subID)

	conn.Unsubscribe(subID)

	got := hub.Broadcast(
		context.Background(),
		rtapi.TopicOperatorsState,
		json.RawMessage(`{}`),
		rtapi.BroadcastFilter{TenantID: "tenant-A"},
	)
	require.Equal(t, 0, got)
}

func TestHub_UnsubscribeUnknownIsNoop(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})

	require.NotPanics(t, func() { conn.Unsubscribe("does-not-exist") })
}

func TestHub_DisconnectByUserClosesAllUserConns(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)

	c1, _ := newTestConnection(t, service.ConnectionConfig{})
	hub.AttachForTest(c1, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	c2, _ := newTestConnection(t, service.ConnectionConfig{})
	hub.AttachForTest(c2, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	other, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { other.Close(rtapi.CloseNormal) })
	hub.AttachForTest(other, rtapi.Claims{
		UserID: "u2", TenantID: "tenant-A", Roles: []string{"admin"},
	})

	hub.DisconnectByUser(context.Background(), "tenant-A", "u1")

	// u1's two connections must be Closed; u2 untouched.
	require.True(t, isConnClosed(c1), "c1 should be closed")
	require.True(t, isConnClosed(c2), "c2 should be closed")
	require.False(t, isConnClosed(other), "other tenant/user should remain open")

	// Hub must drop the closed conns from its registries — Stats
	// should see only `other`.
	stats := hub.Stats()
	require.Equal(t, 1, stats.Connections)
}

func TestHub_DisconnectByUserNoMatch(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})

	require.NotPanics(t, func() {
		hub.DisconnectByUser(context.Background(), "tenant-A", "no-such-user")
	})
	require.False(t, isConnClosed(conn))
	require.Equal(t, 1, hub.Stats().Connections)
}

func TestHub_ConnCloseRemovesFromRegistries(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err := conn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	// Conn-initiated close (e.g., reader exit). Hub's registered
	// onClose callback must drop the conn + sub from registries.
	conn.Close(rtapi.CloseNormal)

	stats := hub.Stats()
	require.Equal(t, 0, stats.Connections)
	require.Empty(t, stats.BySubscription)
}

func TestHub_ConnCloseIdempotent_HubCleanupFiresOnce(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})

	conn.Close(rtapi.CloseNormal)
	conn.Close(rtapi.CloseGoingAway) // second call is a no-op

	require.Equal(t, 0, hub.Stats().Connections)
}

func TestHub_ShutdownClosesAllConns(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)

	c1, _ := newTestConnection(t, service.ConnectionConfig{})
	hub.AttachForTest(c1, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	c2, _ := newTestConnection(t, service.ConnectionConfig{})
	hub.AttachForTest(c2, rtapi.Claims{
		UserID: "u2", TenantID: "tenant-B", Roles: []string{"admin"},
	})

	hub.Shutdown()
	require.True(t, isConnClosed(c1))
	require.True(t, isConnClosed(c2))

	// Idempotent — second Shutdown is a no-op.
	require.NotPanics(t, func() { hub.Shutdown() })
}

func TestHub_StatsCountsConnsAndSubscriptions(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)

	c1, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { c1.Close(rtapi.CloseNormal) })
	hub.AttachForTest(c1, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err := c1.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)
	_, err = c1.Subscribe(rtapi.TopicDialerQueue, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	c2, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { c2.Close(rtapi.CloseNormal) })
	hub.AttachForTest(c2, rtapi.Claims{
		UserID: "u2", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err = c2.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)
	_, err = c2.Subscribe(rtapi.TopicDialerQueue, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	c3, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { c3.Close(rtapi.CloseNormal) })
	hub.AttachForTest(c3, rtapi.Claims{
		UserID: "u3", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err = c3.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	// 3 conns, 5 subs across 2 topics.
	stats := hub.Stats()
	require.Equal(t, 3, stats.Connections)
	require.Equal(t, 3, stats.BySubscription[rtapi.TopicOperatorsState])
	require.Equal(t, 2, stats.BySubscription[rtapi.TopicDialerQueue])
}

func TestHub_BroadcastIncrementsMetrics(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hubMetrics := service.RegisterHubMetrics(reg)
	hub := service.NewHub(nil, hubMetrics, service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err := conn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)

	got := hub.Broadcast(
		context.Background(),
		rtapi.TopicOperatorsState,
		json.RawMessage(`{}`),
		rtapi.BroadcastFilter{TenantID: "tenant-A"},
	)
	require.Equal(t, 1, got)

	// Gauges: connections=1 (one attached conn), subscriptions{topic}=1 (one Subscribe).
	require.InDelta(t, 1.0, gaugeValueFromGather(t, reg, "realtime_hub_connections", nil), 0.0001)
	require.InDelta(t, 1.0, gaugeValueFromGather(t, reg, "realtime_hub_subscriptions",
		map[string]string{"topic": string(rtapi.TopicOperatorsState)}), 0.0001)
	// Counters: broadcasts_total{topic}=1, broadcast_fanout_total{topic}=1
	// (one Broadcast, one matching subscriber).
	require.InDelta(t, 1.0, counterValueFromGather(t, reg, "realtime_hub_broadcasts_total",
		map[string]string{"topic": string(rtapi.TopicOperatorsState)}), 0.0001)
	require.InDelta(t, 1.0, counterValueFromGather(t, reg, "realtime_hub_broadcast_fanout_total",
		map[string]string{"topic": string(rtapi.TopicOperatorsState)}), 0.0001)
}

func TestHub_SubscribeFailureIncrementsMetric(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hubMetrics := service.RegisterHubMetrics(reg)
	hub := service.NewHub(nil, hubMetrics, service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)

	conn, _ := newTestConnection(t, service.ConnectionConfig{})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })
	// operator may NOT subscribe to TopicOperatorsState (admin/supervisor only).
	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"operator"},
	})

	_, err := conn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.ErrorIs(t, err, service.ErrTopicForbidden)

	// Counter must record the rejection with reason="forbidden".
	require.InDelta(t, 1.0, counterValueFromGather(t, reg, "realtime_hub_subscribe_failures_total",
		map[string]string{
			"topic":  string(rtapi.TopicOperatorsState),
			"reason": "forbidden",
		}), 0.0001)
}

func TestHub_ConnectViaWSConn(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)

	fake := newFakeWSConn()
	claims := rtapi.Claims{UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"}}
	apiConn, err := hub.Connect(context.Background(), fake, claims)
	require.NoError(t, err)
	require.NotNil(t, apiConn)
	require.NotEmpty(t, apiConn.ID())
	require.Equal(t, claims, apiConn.Claims())

	subID, err := apiConn.Subscribe(rtapi.TopicOperatorsState, rtapi.SubscriptionFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, subID)

	apiConn.Close(rtapi.CloseNormal)
	require.Equal(t, 0, hub.Stats().Connections)
}

func TestHub_ConnectRejectsZeroClaims(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	fake := newFakeWSConn()
	_, err := hub.Connect(context.Background(), fake, rtapi.Claims{})
	require.ErrorIs(t, err, service.ErrAuthRequired)
}

func TestHub_RegisterHubMetrics_NilRegistererPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { service.RegisterHubMetrics(nil) })
}

func TestHub_NilLoggerOK(t *testing.T) {
	t.Parallel()

	// Smoke: NewHub with nil logger must yield a usable Hub.
	hub := service.NewHub(nil, nil, service.NewTopicRBAC())
	defer hub.Shutdown()
	require.NotNil(t, hub)
	require.Equal(t, 0, hub.Stats().Connections)
}

func TestHub_NewHub_NilRBACPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = service.NewHub(nil, nil, nil)
	})
}

// TestHub_BroadcastDoesNotDeadlockOnCriticalOverflow is the regression
// guard for the Plan 11.2 Task 2 review-found CRITICAL bug:
// Broadcast(RLock) → Send → sendCritical → Close → onConnClose(Lock)
// would deadlock the same goroutine. The two-phase iterate-then-send
// fix in Broadcast (release lock before delivery) makes this safe.
//
// Repro: fill a connection's criticalCh to capacity, then Broadcast a
// critical-class topic to that connection. Without the fix the test
// hangs forever (caught by the bounded done channel + time.After).
func TestHub_BroadcastDoesNotDeadlockOnCriticalOverflow(t *testing.T) {
	t.Parallel()

	hub := newTestHub(t)
	conn, fake := newTestConnection(t, service.ConnectionConfig{
		WriteBufferSize: 16, // telemetry; criticalQueueSize is 32 internally
	})
	t.Cleanup(func() { conn.Close(rtapi.CloseNormal) })

	hub.AttachForTest(conn, rtapi.Claims{
		UserID: "u1", TenantID: "tenant-A", Roles: []string{"admin"},
	})
	_, err := conn.Subscribe(rtapi.TopicCallEvents, rtapi.SubscriptionFilter{CallID: "c1"})
	require.NoError(t, err)

	// Block the writer so criticalCh actually fills.
	fake.BlockWrites()

	// Fill criticalCh to capacity (32 slots).
	for range 32 {
		conn.Send(rtapi.Frame{Type: rtapi.FrameEvent, Topic: rtapi.TopicCallEvents})
	}

	// Now Broadcast a critical-class frame. The 33rd attempted enqueue
	// should overflow, trigger Close, and (with the fix) NOT deadlock.
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Broadcast(t.Context(),
			rtapi.TopicCallEvents,
			[]byte(`{"v":1}`),
			rtapi.BroadcastFilter{TenantID: "tenant-A", CallID: "c1"},
		)
	}()

	select {
	case <-done:
		// Pass: Broadcast returned within bound.
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast deadlocked under critical-queue overflow")
	}

	fake.UnblockWrites()
}

// drainOneFrame consumes one frame from the connection's writer queue
// without spawning runWriter. Returns nil if no frame is queued.
//
// We use the package-internal helper conn.drainSendOnce — exported to
// the test layer via *Connection.DrainSendForTest, defined in
// connection_test_helpers.go (test-only build tag).
func drainOneFrame(_ *testing.T, conn *service.Connection) *rtapi.Frame {
	return conn.DrainSendForTest()
}

// isConnClosed reports whether the connection has entered its closed
// state. The Connection exposes this via DroppedFrames-style probe
// — but for our purposes the simplest check is to send one frame and
// observe that Send is a no-op (close path checks closed.Load).
//
// Direct check via the package-internal IsClosedForTest helper.
func isConnClosed(conn *service.Connection) bool {
	return conn.IsClosedForTest()
}

// gaugeValueFromGather pulls a single gauge value from the registry.
// labels narrows to a specific label set; pass nil for the unlabelled
// (0-d) case.
func gaugeValueFromGather(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.Metric {
			if matchesAllLabels(m, labels) {
				if g := m.GetGauge(); g != nil {
					return g.GetValue()
				}
				return 0
			}
		}
	}
	return 0
}

// counterValueFromGather pulls a single counter value from the
// registry. Sibling of gaugeValueFromGather; the only difference is
// reading m.GetCounter() instead of m.GetGauge().
func counterValueFromGather(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.Metric {
			if matchesAllLabels(m, labels) {
				if c := m.GetCounter(); c != nil {
					return c.GetValue()
				}
				return 0
			}
		}
	}
	return 0
}

func matchesAllLabels(m *dto.Metric, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

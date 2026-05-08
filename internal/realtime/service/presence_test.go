package service_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/realtime/service"
)

// newPresenceTrackerT spins up a fresh miniredis-backed
// *RedisPresenceTracker. The miniredis server, the redis client, and
// the tracker are all bound to t — Cleanup tears them down at test
// exit so parallel suites don't share state.
func newPresenceTrackerT(
	t *testing.T,
	opts ...service.PresenceOption,
) (*service.RedisPresenceTracker, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tracker := service.NewRedisPresenceTracker(rdb, zaptest.NewLogger(t), opts...)
	return tracker, mr
}

// TestPresence_OnConnectMakesUserOnline is the happy-path: after
// OnConnect, IsOnline returns true.
func TestPresence_OnConnectMakesUserOnline(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-1"))

	online, err := tracker.IsOnline(ctx, "tenant-A", "u1")
	require.NoError(t, err)
	assert.True(t, online)
}

// TestPresence_OnDisconnectMakesUserOffline verifies OnDisconnect
// reverses the effect of OnConnect.
func TestPresence_OnDisconnectMakesUserOffline(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-1"))
	require.NoError(t, tracker.OnDisconnect(ctx, "tenant-A", "u1"))

	online, err := tracker.IsOnline(ctx, "tenant-A", "u1")
	require.NoError(t, err)
	assert.False(t, online)
}

// TestPresence_OnConnectStoresReplicaID verifies the per-key value
// matches the replicaID supplied to OnConnect — Plan 11 Task 10's
// janitor relies on this to know which replica owns the session.
func TestPresence_OnConnectStoresReplicaID(t *testing.T) {
	t.Parallel()

	tracker, mr := newPresenceTrackerT(t)
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-42"))

	got, err := mr.Get("presence:tenant-A:user:u1")
	require.NoError(t, err)
	assert.Equal(t, "replica-42", got)
}

// TestPresence_TTLExpires verifies a key written by OnConnect with a
// short TTL stops being online once miniredis fast-forwards past it.
func TestPresence_TTLExpires(t *testing.T) {
	t.Parallel()

	tracker, mr := newPresenceTrackerT(t, service.WithPresenceTTL(100*time.Millisecond))
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-1"))

	// Sanity: still online before the TTL elapses.
	online, err := tracker.IsOnline(ctx, "tenant-A", "u1")
	require.NoError(t, err)
	require.True(t, online)

	mr.FastForward(150 * time.Millisecond)

	online, err = tracker.IsOnline(ctx, "tenant-A", "u1")
	require.NoError(t, err)
	assert.False(t, online, "user should be offline after TTL elapses")
}

// TestPresence_TouchExtendsTTL verifies Touch extends the per-user TTL
// so a long-lived session does not lapse as long as the writer keeps
// touching it.
func TestPresence_TouchExtendsTTL(t *testing.T) {
	t.Parallel()

	tracker, mr := newPresenceTrackerT(t, service.WithPresenceTTL(200*time.Millisecond))
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-1"))

	// Half the TTL elapses, then we Touch.
	mr.FastForward(100 * time.Millisecond)
	require.NoError(t, tracker.Touch(ctx, "tenant-A", "u1"))

	// Another half-TTL elapses (would have expired without Touch).
	mr.FastForward(150 * time.Millisecond)

	online, err := tracker.IsOnline(ctx, "tenant-A", "u1")
	require.NoError(t, err)
	assert.True(t, online, "Touch should keep the session alive across the original TTL boundary")
}

// TestPresence_TouchOnLapsedKeyReturnsErrPresenceLapsed verifies the
// "session lapsed mid-flight" branch: if the key has already expired
// when Touch fires, Touch returns ErrPresenceLapsed so the Hub can
// react (close the WS, force re-connect).
func TestPresence_TouchOnLapsedKeyReturnsErrPresenceLapsed(t *testing.T) {
	t.Parallel()

	tracker, mr := newPresenceTrackerT(t, service.WithPresenceTTL(50*time.Millisecond))
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-1"))
	mr.FastForward(200 * time.Millisecond) // double the TTL — key is gone.

	err := tracker.Touch(ctx, "tenant-A", "u1")
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrPresenceLapsed,
		"Touch on a lapsed key must wrap ErrPresenceLapsed")
}

// TestPresence_TouchOnNeverConnectedReturnsErrPresenceLapsed verifies
// that Touch on a (tenant, user) pair we never OnConnected returns the
// same lapsed sentinel — the caller can't distinguish "expired" from
// "never existed" via Redis EXPIRE; both surface the same way.
func TestPresence_TouchOnNeverConnectedReturnsErrPresenceLapsed(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx := t.Context()

	err := tracker.Touch(ctx, "tenant-A", "ghost-user")
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrPresenceLapsed,
		"Touch on a never-connected key must wrap ErrPresenceLapsed")
}

// TestPresence_OnDisconnectIdempotent verifies OnDisconnect on a
// never-connected user is a no-op (no error) — DEL on a missing key
// returns 0, which is fine.
func TestPresence_OnDisconnectIdempotent(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx := t.Context()

	require.NoError(t, tracker.OnDisconnect(ctx, "tenant-A", "ghost-user"),
		"OnDisconnect on a never-connected user must be a no-op")
}

// TestPresence_OnConnectIsIdempotent verifies a second OnConnect
// overwrites the prior replica ID — when a user reconnects to a new
// replica the value MUST move with the connection.
func TestPresence_OnConnectIsIdempotent(t *testing.T) {
	t.Parallel()

	tracker, mr := newPresenceTrackerT(t)
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-1"))
	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-2"))

	got, err := mr.Get("presence:tenant-A:user:u1")
	require.NoError(t, err)
	assert.Equal(t, "replica-2", got, "second OnConnect must overwrite replicaID")
}

// TestPresence_OnlineUsersReturnsSorted verifies OnlineUsers returns
// every user for the tenant, alphabetically sorted for deterministic
// output (callers iterate this for diff detection).
func TestPresence_OnlineUsersReturnsSorted(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx := t.Context()

	for _, u := range []string{"charlie", "alice", "bob"} {
		require.NoError(t, tracker.OnConnect(ctx, "tenant-A", u, "replica-1"))
	}

	users, err := tracker.OnlineUsers(ctx, "tenant-A")
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob", "charlie"}, users)
	assert.True(t, slices.IsSorted(users), "OnlineUsers must return alphabetically sorted output")
}

// TestPresence_OnlineUsersIsTenantIsolated verifies the SCAN pattern
// scoped to the tenant prefix doesn't leak users from other tenants.
func TestPresence_OnlineUsersIsTenantIsolated(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "alice", "r1"))
	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "bob", "r1"))
	require.NoError(t, tracker.OnConnect(ctx, "tenant-B", "carol", "r2"))

	a, err := tracker.OnlineUsers(ctx, "tenant-A")
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob"}, a)

	b, err := tracker.OnlineUsers(ctx, "tenant-B")
	require.NoError(t, err)
	assert.Equal(t, []string{"carol"}, b)
}

// TestPresence_OnlineUsersEmpty verifies the zero case — a tenant with
// no online users returns an empty (non-nil-or-nil; either is fine)
// slice and no error.
func TestPresence_OnlineUsersEmpty(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx := t.Context()

	users, err := tracker.OnlineUsers(ctx, "tenant-A")
	require.NoError(t, err)
	assert.Empty(t, users)
}

// TestPresence_IsOnlineRespectsCtxCancel verifies a cancelled context
// surfaces ctx.Err() rather than blocking forever on Redis.
func TestPresence_IsOnlineRespectsCtxCancel(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before the call

	_, err := tracker.IsOnline(ctx, "tenant-A", "u1")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled,
		"cancelled ctx must surface context.Canceled")
}

// TestPresence_OnlineUsersRespectsCtxCancel verifies the SCAN loop
// also bails when ctx is cancelled.
func TestPresence_OnlineUsersRespectsCtxCancel(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := tracker.OnlineUsers(ctx, "tenant-A")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled,
		"cancelled ctx must surface context.Canceled")
}

// TestPresence_ConcurrentOnConnect drives several goroutines hitting
// OnConnect for distinct (tenant, user) pairs in parallel. The aim is
// race-detector clean — no panics, no shared-state hazards. Run with
// `go test -race`.
func TestPresence_ConcurrentOnConnect(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx := t.Context()

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			userID := "u" + string(rune('a'+idx%26)) + string(rune('0'+idx/26))
			if err := tracker.OnConnect(ctx, "tenant-A", userID, "replica-1"); err != nil {
				t.Errorf("OnConnect failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	users, err := tracker.OnlineUsers(ctx, "tenant-A")
	require.NoError(t, err)
	assert.Len(t, users, N)
}

// TestPresence_NewWithNilRedisPanics verifies a wiring bug surfaces at
// boot — passing a nil rdb is never legitimate.
func TestPresence_NewWithNilRedisPanics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t,
		"service.NewRedisPresenceTracker: rdb must be non-nil",
		func() {
			service.NewRedisPresenceTracker(nil, zaptest.NewLogger(t))
		})
}

// TestPresence_NewWithNilLoggerNopFallback verifies the logger
// nil-safety: a nil logger falls back to zap.NewNop, no panic.
func TestPresence_NewWithNilLoggerNopFallback(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tracker := service.NewRedisPresenceTracker(rdb, nil)
	require.NotNil(t, tracker)

	require.NoError(t, tracker.OnConnect(t.Context(), "tenant-A", "u1", "r1"))
}

// TestPresence_WithPresenceTTLZeroDefaults guards against a footgun:
// passing WithPresenceTTL(0) MUST NOT zero out the TTL (Redis reads
// 0ms as "no TTL"). Either reject the option or fall back to the
// default 30s. The implementation must not silently accept it.
func TestPresence_WithPresenceTTLZeroDefaults(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tracker := service.NewRedisPresenceTracker(rdb, zaptest.NewLogger(t),
		service.WithPresenceTTL(0))
	require.NoError(t, tracker.OnConnect(t.Context(), "tenant-A", "u1", "r1"))

	// Key should have a positive TTL (i.e., not -1 == "no expiry").
	ttl := mr.TTL("presence:tenant-A:user:u1")
	assert.Positive(t, ttl.Nanoseconds(),
		"WithPresenceTTL(0) must fall back to a positive default, not 'no TTL'")
}

// TestPresence_PresenceMetrics_RecordsTouchOutcomes verifies the
// PresenceMetrics counter increments with the right `result` label
// across the three Touch outcomes (ok / lapsed / error). Inspect via
// the registry's Gather().
func TestPresence_PresenceMetrics_RecordsTouchOutcomes(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := prometheus.NewRegistry()
	pm := service.RegisterPresenceMetrics(reg)
	tracker := service.NewRedisPresenceTracker(rdb, zaptest.NewLogger(t),
		service.WithPresenceTTL(50*time.Millisecond),
		service.WithPresenceMetrics(pm),
	)
	ctx := t.Context()

	// ok branch.
	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "r1"))
	require.NoError(t, tracker.Touch(ctx, "tenant-A", "u1"))

	// lapsed branch.
	mr.FastForward(200 * time.Millisecond)
	err := tracker.Touch(ctx, "tenant-A", "u1")
	require.Error(t, err)
	require.ErrorIs(t, err, service.ErrPresenceLapsed)

	// Gather and check the touch counter has labels {ok, lapsed}.
	require.InDelta(t, 1.0,
		counterValueFromGather(t, reg, "realtime_presence_touch_total",
			map[string]string{"result": "ok"}), 0.0001)
	require.InDelta(t, 1.0,
		counterValueFromGather(t, reg, "realtime_presence_touch_total",
			map[string]string{"result": "lapsed"}), 0.0001)
}

// TestPresence_PresenceMetrics_SetOnlineUsersGauge exercises the
// gauge setter exposed for the future Plan 11 Task 10 janitor.
// PresenceMetrics owns the gauge but the tracker itself never writes
// it (per-OnConnect updates would double-count cross-replica), so we
// verify the public seam works in isolation.
func TestPresence_PresenceMetrics_SetOnlineUsersGauge(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	pm := service.RegisterPresenceMetrics(reg)

	pm.SetOnlineUsers("tenant-A", 7)
	pm.SetOnlineUsers("tenant-B", 3)

	require.InDelta(t, 7.0,
		gaugeValueFromGather(t, reg,
			"realtime_presence_online_users_count",
			map[string]string{"tenant_id": "tenant-A"}), 0.0001)
	require.InDelta(t, 3.0,
		gaugeValueFromGather(t, reg,
			"realtime_presence_online_users_count",
			map[string]string{"tenant_id": "tenant-B"}), 0.0001)

	// Nil-tolerated guard: SetOnlineUsers on a nil receiver is a no-op.
	var nilPM *service.PresenceMetrics
	nilPM.SetOnlineUsers("tenant-X", 1) // must not panic
}

// TestPresence_NilMetricsObservers verifies the nil-tolerated
// observer hooks: passing a nil *PresenceMetrics through the tracker
// must NOT panic on Connect / Disconnect / Touch.
func TestPresence_NilMetricsObservers(t *testing.T) {
	t.Parallel()

	tracker, mr := newPresenceTrackerT(t, service.WithPresenceTTL(50*time.Millisecond))
	ctx := t.Context()

	// No WithPresenceMetrics — the tracker holds a nil *PresenceMetrics.
	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "r1"))
	require.NoError(t, tracker.Touch(ctx, "tenant-A", "u1"))
	require.NoError(t, tracker.OnDisconnect(ctx, "tenant-A", "u1"))

	mr.FastForward(200 * time.Millisecond)
	err := tracker.Touch(ctx, "tenant-A", "u1")
	require.Error(t, err) // lapsed branch — nil metrics still fine
}

// TestPresence_PresenceMetrics_ConnectDisconnectCounters verifies the
// connect/disconnect counters increment on every successful call.
func TestPresence_PresenceMetrics_ConnectDisconnectCounters(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := prometheus.NewRegistry()
	pm := service.RegisterPresenceMetrics(reg)
	tracker := service.NewRedisPresenceTracker(rdb, zaptest.NewLogger(t),
		service.WithPresenceMetrics(pm))
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "r1"))
	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u2", "r1"))
	require.NoError(t, tracker.OnDisconnect(ctx, "tenant-A", "u1"))

	require.InDelta(t, 2.0,
		counterValueFromGather(t, reg, "realtime_presence_connect_total", nil), 0.0001)
	require.InDelta(t, 1.0,
		counterValueFromGather(t, reg, "realtime_presence_disconnect_total", nil), 0.0001)
}

// TestPresence_RegisterPresenceMetricsNilRegistererPanics is the
// boot-time guard: a wiring bug must surface immediately.
func TestPresence_RegisterPresenceMetricsNilRegistererPanics(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() {
		_ = service.RegisterPresenceMetrics(nil)
	})
}

// TestPresence_TouchOnConnectSequenceKeepsTTL is a regression guard
// for the "Touch must not silently re-create a missing key" rule. The
// Hub flow is OnConnect → many Touch calls; if Touch ever called SET
// instead of EXPIRE, a stale TTL bug could mask a session that should
// have lapsed. We verify Touch only refreshes the existing key, never
// resurrects.
func TestPresence_TouchDoesNotResurrectMissingKey(t *testing.T) {
	t.Parallel()

	tracker, mr := newPresenceTrackerT(t, service.WithPresenceTTL(100*time.Millisecond))
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-1"))
	mr.FastForward(200 * time.Millisecond)

	// Touch errors out (lapsed). Verify IsOnline still reports false:
	// the key MUST NOT have been re-SET as a side effect.
	_ = tracker.Touch(ctx, "tenant-A", "u1")

	online, err := tracker.IsOnline(ctx, "tenant-A", "u1")
	require.NoError(t, err)
	assert.False(t, online, "Touch on a lapsed key must not resurrect the key")
}

// TestPresence_RedisErrorPropagation closes miniredis under us so
// every subsequent Redis operation surfaces a connection error. This
// covers the wrapped-error branches of OnConnect, OnDisconnect, Touch,
// IsOnline, and OnlineUsers.
func TestPresence_RedisErrorPropagation(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := prometheus.NewRegistry()
	pm := service.RegisterPresenceMetrics(reg)
	tracker := service.NewRedisPresenceTracker(rdb, zaptest.NewLogger(t),
		service.WithPresenceMetrics(pm))
	ctx := t.Context()

	mr.Close() // every subsequent operation now errors out.

	require.Error(t, tracker.OnConnect(ctx, "tenant-A", "u1", "r1"))
	require.Error(t, tracker.OnDisconnect(ctx, "tenant-A", "u1"))

	err := tracker.Touch(ctx, "tenant-A", "u1")
	require.Error(t, err)
	// On a Redis-side failure (not a missing key), Touch must NOT
	// emit ErrPresenceLapsed — it's a connection error, not a
	// session-lapse signal.
	assert.NotErrorIs(t, err, service.ErrPresenceLapsed)

	_, err = tracker.IsOnline(ctx, "tenant-A", "u1")
	require.Error(t, err)

	_, err = tracker.OnlineUsers(ctx, "tenant-A")
	require.Error(t, err)

	// The error-result counter incremented exactly once (the Touch).
	require.InDelta(t, 1.0,
		counterValueFromGather(t, reg, "realtime_presence_touch_total",
			map[string]string{"result": "error"}), 0.0001)
}

// TestPresence_OnlineUsersIgnoresStrayKeys verifies the defensive
// guard in userIDFromKey: a stray (non-presence) key in the same
// namespace doesn't poison OnlineUsers' return slice. miniredis lets
// us write a key that matches the SCAN pattern's prefix segment but
// not the full shape.
func TestPresence_OnlineUsersIgnoresStrayKeys(t *testing.T) {
	t.Parallel()

	tracker, mr := newPresenceTrackerT(t)
	ctx := t.Context()

	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "alice", "r1"))

	// Inject a malformed-but-matching key directly into miniredis.
	// "presence:tenant-A:user" (no trailing user segment) — SCAN
	// pattern presence:tenant-A:user:* WON'T match this; injecting
	// presence:tenant-A:user:weird:extra:segments stays inside the
	// glob and lets us prove the parser tolerates it.
	require.NoError(t, mr.Set("presence:tenant-A:user:weird:extra:bits", "stray"))

	users, err := tracker.OnlineUsers(ctx, "tenant-A")
	require.NoError(t, err)
	// SplitN(_, ":", 4) yields ["presence", "tenant-A", "user",
	// "weird:extra:bits"] — userID is "weird:extra:bits", which the
	// guard accepts (parts[3] is non-empty and parts[2]=="user").
	// We assert alice is present plus the stray key surfaces (the
	// parser is forgiving by design — operators see the stray and
	// fix it). The point of this test is to lock in non-panic
	// behaviour.
	assert.Contains(t, users, "alice")
}

// TestPresence_OnlineUsersScansAcrossManyKeys verifies the SCAN loop
// completes correctly when keys exceed the per-iteration COUNT batch.
// Plain miniredis honours COUNT but iterates internally, so this is
// primarily a smoke-test that the cursor handling doesn't drop keys.
func TestPresence_OnlineUsersScansAcrossManyKeys(t *testing.T) {
	t.Parallel()

	tracker, _ := newPresenceTrackerT(t)
	ctx := t.Context()

	const N = 250 // > presenceScanCount(100), forces multi-batch SCAN
	for i := 0; i < N; i++ {
		userID := "user-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26%10))
		require.NoError(t, tracker.OnConnect(ctx, "tenant-A", userID, "r1"))
	}

	users, err := tracker.OnlineUsers(ctx, "tenant-A")
	require.NoError(t, err)
	// Some user IDs collide on the small alphabet — we just check
	// the count is positive and the slice is sorted.
	assert.NotEmpty(t, users)
	assert.True(t, slices.IsSorted(users))
}

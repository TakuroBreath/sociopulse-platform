package realtime_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/internal/realtime"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// TestMain installs goleak as defence-in-depth. The realtime root
// package's Register builds a *service.Hub which itself spawns no
// goroutines — but the test suite exercises the full Module lifecycle
// (Register + Stop), and a regression that adds a stray goroutine
// (e.g. a future presence sweeper started from Register) would silently
// leak without this guard.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeSubscriber satisfies eventbus.Subscriber. The dispatcher is NOT
// constructed inside Module.Register — that lives in cmd/api per the
// plan-11-realtime.md gotcha at line 97 — but Register still wants to
// see a non-nil Subscriber so it can fail fast when the composition
// root forgot to wire one. We never expect Subscribe to be called from
// Register itself.
type fakeSubscriber struct{}

func (fakeSubscriber) Subscribe(_ context.Context, _, _ string, _ func(string, []byte) error) error {
	return nil
}

// newModule constructs a *Module with a fresh per-test prometheus
// registry so test goroutines don't trip over duplicate metric
// registration.
func newModule() *realtime.Module {
	return realtime.New(realtime.Config{Registerer: prometheus.NewRegistry()})
}

// newDeps constructs a modules.Deps suitable for exercising
// realtime.Module.Register. cmd/api in production wires far more
// (Pool, Redis, HTTPRouter, EventBus, Config, Ctx) but Register only
// needs Subscriber + Logger + Locator; everything else stays at zero.
func newDeps(t *testing.T) modules.Deps {
	t.Helper()
	return modules.Deps{
		Logger:     zaptest.NewLogger(t),
		Subscriber: fakeSubscriber{},
		Locator:    modules.NewMapLocator(),
	}
}

// TestModule_RegisterStashesHubInLocator validates the canonical
// Register behaviour: a fully-formed Hub appears under
// rtapi.LocatorHub, the per-connection metrics struct under
// rtapi.LocatorConnectionMetrics, and Register returns nil.
func TestModule_RegisterStashesHubInLocator(t *testing.T) {
	t.Parallel()

	mod := newModule()
	deps := newDeps(t)

	require.NoError(t, mod.Register(deps))

	raw, ok := deps.Locator.Lookup(rtapi.LocatorHub)
	require.True(t, ok, "Hub should be registered in locator under LocatorHub")
	hub, ok := raw.(rtapi.Hub)
	require.True(t, ok, "stored value must satisfy api.Hub")
	require.NotNil(t, hub)
	// Stats works without any connections — exercises that the Hub is real.
	stats := hub.Stats()
	assert.Equal(t, 0, stats.Connections)

	rawMetrics, ok := deps.Locator.Lookup(rtapi.LocatorConnectionMetrics)
	require.True(t, ok, "ConnectionMetrics should be registered in locator")
	require.NotNil(t, rawMetrics)
}

// TestModule_RegisterUsesSuppliedRegistry verifies the module shares
// the supplied registry rather than constructing a private one. This
// is the seam cmd/api exercises so realtime collectors land on /metrics
// alongside HTTP + DB counters.
//
// Detection trick: register the same Hub gauge a SECOND time on the
// supplied registry; if the module truly used this registry, the
// duplicate registration trips AlreadyRegisteredError. The Gauge is
// the simplest collector to assert against (CounterVec entries don't
// surface in Gather() until they take a label value).
func TestModule_RegisterUsesSuppliedRegistry(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	mod := realtime.New(realtime.Config{Registerer: reg})
	deps := newDeps(t)

	require.NoError(t, mod.Register(deps))

	// Help string must match the original collector's exactly so the
	// duplicate-registration check trips with AlreadyRegisteredError
	// instead of a desc-mismatch error.
	dup := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "realtime_hub_connections",
		Help: "Current number of WebSocket connections registered with the realtime Hub.",
	})
	err := reg.Register(dup)
	require.Error(t, err, "Hub connections gauge must already be registered on the supplied registry")

	var arErr prometheus.AlreadyRegisteredError
	require.ErrorAs(t, err, &arErr)
}

// TestModule_RegisterIsIdempotent verifies a second call to Register
// is a no-op (no panic, no duplicate metric registration). Mirrors
// the cross-module idempotency contract documented on Module.Register.
func TestModule_RegisterIsIdempotent(t *testing.T) {
	t.Parallel()

	mod := newModule()
	deps := newDeps(t)

	require.NoError(t, mod.Register(deps))
	require.NoError(t, mod.Register(deps), "second Register must not error")
}

// TestModule_RegisterRequiresSubscriber verifies the up-front guard:
// even though the dispatcher itself is constructed in cmd/api (per
// the plan-11 gotcha), Register still rejects a nil Subscriber so a
// composition-root wiring bug surfaces at boot.
func TestModule_RegisterRequiresSubscriber(t *testing.T) {
	t.Parallel()

	mod := newModule()
	deps := newDeps(t)
	deps.Subscriber = nil

	err := mod.Register(deps)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Subscriber")
}

// TestModule_StopBeforeRegister verifies Stop is safe to call on a
// freshly-constructed Module. Mirrors the dialer.Module.Stop
// idempotency rule.
func TestModule_StopBeforeRegister(t *testing.T) {
	t.Parallel()

	mod := newModule()
	require.NoError(t, mod.Stop(), "Stop on un-registered module must be a no-op")
}

// TestModule_StopAfterRegister verifies Stop closes the Hub after
// Register has wired it. The Hub starts with zero connections so
// Shutdown is a no-op fan-out, but calling Stop must mark the module
// stopped and remain safe under repeat invocation.
func TestModule_StopAfterRegister(t *testing.T) {
	t.Parallel()

	mod := newModule()
	deps := newDeps(t)

	require.NoError(t, mod.Register(deps))
	require.NoError(t, mod.Stop())
	require.NoError(t, mod.Stop(), "second Stop must be a no-op")

	// Verify the Hub remains in the locator (Stop tears down the
	// service but does not unregister it — cmd/api may want to read
	// stats from it during shutdown).
	raw, ok := deps.Locator.Lookup(rtapi.LocatorHub)
	require.True(t, ok)
	require.NotNil(t, raw)
}

// TestModule_RegisterRejectsNilLocator verifies the locator
// dependency is required — Register has no fallback.
func TestModule_RegisterRejectsNilLocator(t *testing.T) {
	t.Parallel()

	mod := newModule()
	deps := newDeps(t)
	deps.Locator = nil

	err := mod.Register(deps)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Locator")
}

// TestModule_Name returns the registry identifier. Exhaustively
// verified at the registry level; the assertion here pins the name so
// a casual rename would surface as a test failure.
func TestModule_Name(t *testing.T) {
	t.Parallel()
	require.Equal(t, "realtime", newModule().Name())
}

// TestModule_NewWithoutRegisterer falls back to a nop registry so
// Register builds the Hub with throw-away metrics rather than
// panicking. Useful in tests where the caller doesn't care about
// inspecting collectors.
func TestModule_NewWithoutRegisterer(t *testing.T) {
	t.Parallel()

	mod := realtime.New(realtime.Config{})
	deps := newDeps(t)

	require.NoError(t, mod.Register(deps))
	raw, ok := deps.Locator.Lookup(rtapi.LocatorHub)
	require.True(t, ok)
	require.NotNil(t, raw)
}

// TestModule_RegisterStashesPresenceTrackerWhenRedisAvailable verifies
// the Plan 11 Task 5 wiring: when Deps.Redis is non-nil, Register
// builds a RedisPresenceTracker and stashes it in the locator under
// rtapi.LocatorPresenceTracker.
func TestModule_RegisterStashesPresenceTrackerWhenRedisAvailable(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	mod := newModule()
	deps := newDeps(t)
	deps.Redis = rdb

	require.NoError(t, mod.Register(deps))

	raw, ok := deps.Locator.Lookup(rtapi.LocatorPresenceTracker)
	require.True(t, ok, "PresenceTracker should be registered when Deps.Redis is wired")
	tracker, ok := raw.(rtapi.PresenceTracker)
	require.True(t, ok, "stored value must satisfy api.PresenceTracker")
	require.NotNil(t, tracker)

	// Smoke-test the wired tracker against the real miniredis so the
	// composition path is exercised end-to-end (not just the locator
	// stash).
	ctx := t.Context()
	require.NoError(t, tracker.OnConnect(ctx, "tenant-A", "u1", "replica-test"))
	online, err := tracker.IsOnline(ctx, "tenant-A", "u1")
	require.NoError(t, err)
	assert.True(t, online)
}

// TestModule_RegisterSkipsPresenceTrackerWhenRedisNil verifies the
// degraded-mode boot path: a nil Deps.Redis is legitimate (test or
// dev), Register logs an INFO and continues without registering the
// tracker. The locator key is absent so downstream consumers can
// detect the no-presence mode.
func TestModule_RegisterSkipsPresenceTrackerWhenRedisNil(t *testing.T) {
	t.Parallel()

	mod := newModule()
	deps := newDeps(t)
	require.Nil(t, deps.Redis, "test fixture should default to nil Redis")

	require.NoError(t, mod.Register(deps))

	_, ok := deps.Locator.Lookup(rtapi.LocatorPresenceTracker)
	assert.False(t, ok, "PresenceTracker should NOT be registered when Deps.Redis is nil")

	// Hub still registers — the absence of presence does not block
	// the rest of realtime composition.
	_, ok = deps.Locator.Lookup(rtapi.LocatorHub)
	assert.True(t, ok, "Hub should still register even without Redis")
}

// TestModule_RegisterPresenceMetricsLandOnSharedRegistry verifies the
// presence collectors are registered on the supplied registry, the
// same way the Hub metrics are. A duplicate-registration probe trips
// AlreadyRegisteredError if the presence counter is on the right
// registerer.
func TestModule_RegisterPresenceMetricsLandOnSharedRegistry(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := prometheus.NewRegistry()
	mod := realtime.New(realtime.Config{Registerer: reg})
	deps := newDeps(t)
	deps.Redis = rdb

	require.NoError(t, mod.Register(deps))

	dup := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "realtime_presence_connect_total",
		Help: "Total OnConnect events recorded by the realtime presence tracker.",
	})
	err := reg.Register(dup)
	require.Error(t, err, "presence connect counter must already be registered")

	var arErr prometheus.AlreadyRegisteredError
	require.ErrorAs(t, err, &arErr)
}

package router_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/internal/telephony/router"
	"github.com/sociopulse/platform/pkg/config"
)

// TestMain enforces goroutine quiescence on package exit. The router itself
// spawns no goroutines in v1 (Start/Stop are no-ops) but the miniredis +
// redis client paths can leak parked goroutines on misuse — goleak catches
// those before they bite a future maintainer.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakePool is a stub Pool that returns a fixed list of healthy nodes. The
// list is mutable via SetHealthy so tests can simulate a node going
// unhealthy mid-test.
type fakePool struct {
	mu      sync.RWMutex
	healthy []string
}

func newFakePool(healthy ...string) *fakePool {
	return &fakePool{healthy: append([]string(nil), healthy...)}
}

func (f *fakePool) HealthyNodes() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]string(nil), f.healthy...)
}

func (f *fakePool) SetHealthy(nodes ...string) { //nolint:unused // reserved for hot-failover tests
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthy = append([]string(nil), nodes...)
}

// newRouterT spins up a router with the given trunk catalog, a fakePool, and
// a miniredis-backed Redis client. Returns the router and the fake pool so
// tests can mutate the healthy-node set mid-test.
func newRouterT(t *testing.T, trunks []config.TrunkConfig, healthy []string, opts ...routerOption) (*router.Router, *fakePool) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pool := newFakePool(healthy...)

	cfg := router.Config{
		Pool:            pool,
		Redis:           rdb,
		BackpressureCap: 60,
		Trunks:          trunks,
		DefaultStrategy: string(api.RouteLeastCost),
		Logger:          zaptest.NewLogger(t),
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	r, err := router.New(cfg)
	require.NoError(t, err)
	t.Cleanup(r.Stop)
	return r, pool
}

type routerOption func(*router.Config)

func withCap(n int) routerOption { return func(c *router.Config) { c.BackpressureCap = n } }
func withDefaultStrategy(s string) routerOption {
	return func(c *router.Config) { c.DefaultStrategy = s }
}
func withMetrics(m *router.Metrics) routerOption     { return func(c *router.Config) { c.Metrics = m } } //nolint:unused // reserved for metrics tests
func withTrunks(t []config.TrunkConfig) routerOption { return func(c *router.Config) { c.Trunks = t } }  //nolint:unused // reserved for runtime catalog tests

// New ---------------------------------------------------------------------

func TestRouter_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	_, err := router.New(router.Config{
		Pool:  nil,
		Redis: rdb,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Pool")
}

func TestRouter_New_RejectsNilRedis(t *testing.T) {
	t.Parallel()
	_, err := router.New(router.Config{
		Pool:  newFakePool("n1"),
		Redis: nil,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Redis")
}

func TestRouter_New_ToleratesEmptyTrunks(t *testing.T) {
	t.Parallel()
	r, _ := newRouterT(t, nil, []string{"n1"})
	// Select must return ErrNoTrunkAvailable — empty catalog is a
	// degraded but valid state during operator wiring.
	_, err := r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

func TestRouter_Start_Stop_AreNoops(t *testing.T) {
	t.Parallel()
	r, _ := newRouterT(t, nil, nil)
	require.NoError(t, r.Start(context.Background()))
	r.Stop() // idempotent / safe
}

// Select happy paths ------------------------------------------------------

func TestRouter_Select_PicksHealthyNode(t *testing.T) {
	t.Parallel()
	trunks := []config.TrunkConfig{
		{ID: "primary", SIPGateway: "gw-primary", CapacityChannels: 60, CostPerMinuteRub: 0.02},
	}
	r, _ := newRouterT(t, trunks, []string{"node-1", "node-2"})

	got, err := r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.NoError(t, err)
	require.Equal(t, "primary", got.TrunkID)
	require.Contains(t, []string{"node-1", "node-2"}, got.FSNode)
	require.Equal(t, string(api.RouteLeastCost), got.Reason)
}

func TestRouter_Select_RejectsWhenNoHealthyNode(t *testing.T) {
	t.Parallel()
	trunks := []config.TrunkConfig{
		{ID: "primary", SIPGateway: "gw-primary", CapacityChannels: 60, CostPerMinuteRub: 0.02},
	}
	// Healthy=empty — every trunk is matched by the strategy but no FS
	// node can host the call.
	r, _ := newRouterT(t, trunks, nil)

	_, err := r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

func TestRouter_Select_RejectsWhenAllNodesAtCap(t *testing.T) {
	t.Parallel()
	trunks := []config.TrunkConfig{
		{ID: "primary", SIPGateway: "gw-primary", CapacityChannels: 60, CostPerMinuteRub: 0.02},
	}
	// Cap = 1 so a single Acquire fills every node.
	r, _ := newRouterT(t, trunks, []string{"n1", "n2"}, withCap(1))
	ctx := context.Background()
	req := api.SelectRequest{TenantID: uuid.New(), OperatorID: uuid.New()}

	// Two successes — each fills one node.
	first, err := r.Select(ctx, req)
	require.NoError(t, err)
	second, err := r.Select(ctx, req)
	require.NoError(t, err)
	require.NotEqual(t, first.FSNode, second.FSNode, "two acquires should land on different nodes")

	// Third must fail — both nodes at cap.
	_, err = r.Select(ctx, req)
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

func TestRouter_Select_RespectsRequestedStrategy(t *testing.T) {
	t.Parallel()
	trunks := []config.TrunkConfig{
		{ID: "cheap", SIPGateway: "gw-cheap", CapacityChannels: 60, CostPerMinuteRub: 0.01, Weight: 10},
		{ID: "expensive", SIPGateway: "gw-expensive", CapacityChannels: 60, CostPerMinuteRub: 0.10, Weight: 10},
	}
	// Default = least_cost; request = round_robin → must alternate by ID
	// regardless of cost.
	r, _ := newRouterT(t, trunks, []string{"n1"})
	ctx := context.Background()

	picks := make([]string, 0, 4)
	for range 4 {
		// Need fresh capacity each time; cap defaults to 60 so 4 picks fit.
		got, err := r.Select(ctx, api.SelectRequest{
			TenantID:   uuid.New(),
			OperatorID: uuid.New(),
			Strategy:   api.RouteRoundRobin,
		})
		require.NoError(t, err)
		picks = append(picks, got.TrunkID)
	}
	// RoundRobin sorts by ID lex: "cheap" < "expensive"
	require.Equal(t, []string{"cheap", "expensive", "cheap", "expensive"}, picks)
}

func TestRouter_Select_FallsBackToDefaultStrategy(t *testing.T) {
	t.Parallel()
	trunks := []config.TrunkConfig{
		{ID: "cheap", SIPGateway: "gw-cheap", CapacityChannels: 60, CostPerMinuteRub: 0.01},
		{ID: "expensive", SIPGateway: "gw-expensive", CapacityChannels: 60, CostPerMinuteRub: 0.10},
	}
	r, _ := newRouterT(t, trunks, []string{"n1"}, withDefaultStrategy(string(api.RouteLeastCost)))

	got, err := r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   "", // empty → fall back to DefaultStrategy
	})
	require.NoError(t, err)
	require.Equal(t, "cheap", got.TrunkID, "default least_cost should pick the cheap trunk")
	require.Equal(t, string(api.RouteLeastCost), got.Reason)
}

func TestRouter_Select_UnknownStrategyFallsBackToLeastCost(t *testing.T) {
	t.Parallel()
	trunks := []config.TrunkConfig{
		{ID: "cheap", SIPGateway: "gw-cheap", CapacityChannels: 60, CostPerMinuteRub: 0.01},
		{ID: "expensive", SIPGateway: "gw-expensive", CapacityChannels: 60, CostPerMinuteRub: 0.10},
	}
	// Default = empty too — must use the global least_cost fallback.
	r, _ := newRouterT(t, trunks, []string{"n1"}, withDefaultStrategy(""))

	got, err := r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RoutingStrategy("totally-bogus"),
	})
	require.NoError(t, err)
	require.Equal(t, "cheap", got.TrunkID)
	require.Equal(t, string(api.RouteLeastCost), got.Reason)
}

// Trunk-config mapping ----------------------------------------------------

func TestRouter_New_MapsCapacityToActive(t *testing.T) {
	t.Parallel()
	trunks := []config.TrunkConfig{
		// CapacityChannels = 0 → inactive even with a gateway set.
		{ID: "drained", SIPGateway: "gw-drained", CapacityChannels: 0, CostPerMinuteRub: 0.001},
		// SIPGateway empty → inactive even with capacity > 0.
		{ID: "no-gw", SIPGateway: "", CapacityChannels: 60, CostPerMinuteRub: 0.001},
		// Active trunk.
		{ID: "ok", SIPGateway: "gw-ok", CapacityChannels: 60, CostPerMinuteRub: 0.05},
	}
	r, _ := newRouterT(t, trunks, []string{"n1"})

	got, err := r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.NoError(t, err)
	require.Equal(t, "ok", got.TrunkID, "drained and no-gw must be filtered out by the New-time mapping")
}

// ReleaseChannel ----------------------------------------------------------

func TestRouter_ReleaseChannel_DecrementsCounter(t *testing.T) {
	t.Parallel()
	trunks := []config.TrunkConfig{
		{ID: "primary", SIPGateway: "gw-primary", CapacityChannels: 60, CostPerMinuteRub: 0.02},
	}
	r, _ := newRouterT(t, trunks, []string{"n1"}, withCap(1))
	ctx := context.Background()
	req := api.SelectRequest{TenantID: uuid.New(), OperatorID: uuid.New()}

	// Fill cap.
	got, err := r.Select(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "n1", got.FSNode)

	// Second Select must fail.
	_, err = r.Select(ctx, req)
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)

	// Release one slot — third Select should succeed.
	require.NoError(t, r.ReleaseChannel(ctx, "n1"))
	got, err = r.Select(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "n1", got.FSNode)
}

func TestRouter_ReleaseChannel_OverReleaseClampsAtZero(t *testing.T) {
	t.Parallel()
	r, _ := newRouterT(t, nil, []string{"n1"})
	ctx := context.Background()

	// Release without prior acquire — Lua script clamps at 0.
	require.NoError(t, r.ReleaseChannel(ctx, "n1"))
	require.NoError(t, r.ReleaseChannel(ctx, "n1"))
	bp := r.Backpressure()
	got, err := bp.Get(ctx, "n1")
	require.NoError(t, err)
	require.Equal(t, 0, got)
}

// Pool stub ---------------------------------------------------------------

func TestRouter_Select_RespectsHealthySetChange(t *testing.T) {
	t.Parallel()
	trunks := []config.TrunkConfig{
		{ID: "primary", SIPGateway: "gw-primary", CapacityChannels: 60, CostPerMinuteRub: 0.02},
	}
	r, pool := newRouterT(t, trunks, []string{"n1", "n2"})
	ctx := context.Background()
	req := api.SelectRequest{TenantID: uuid.New(), OperatorID: uuid.New()}

	// Initially: both nodes healthy.
	got, err := r.Select(ctx, req)
	require.NoError(t, err)
	require.Contains(t, []string{"n1", "n2"}, got.FSNode)

	// Now n1 fails health probe — only n2 is selectable.
	pool.SetHealthy("n2")
	got, err = r.Select(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "n2", got.FSNode)

	// Both fail — no candidate left.
	pool.SetHealthy()
	_, err = r.Select(ctx, req)
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

// Compile-time interface assertion (mirrors the one in router.go;
// duplicated in the test package so tests catch interface drift even when
// the production file is excluded from a build).
var _ api.Router = (*router.Router)(nil)

// Sanity check that ErrNoTrunkAvailable is errors.Is-friendly.
func TestErrNoTrunkAvailable_IsUsable(t *testing.T) {
	t.Parallel()
	wrapped := errors.New("wrapped")
	combined := errors.Join(router.ErrNoTrunkAvailable, wrapped)
	require.ErrorIs(t, combined, router.ErrNoTrunkAvailable)
}

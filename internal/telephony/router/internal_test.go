package router

// Package-internal tests that exercise corners not reachable from the public
// API in v1. The intersection branch of candidateNodes, for example, is
// dead from the public wiring (cfg.Telephony.Trunks lacks per-trunk
// NodeAddrs in v1) but slated to come alive in Plan 13/14 when the trunk
// catalog moves to Postgres. Keeping the branch covered avoids a stealth
// regression when that lands.

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

type fakePoolInternal struct {
	mu      sync.RWMutex
	healthy []string
}

func (f *fakePoolInternal) HealthyNodes() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]string(nil), f.healthy...)
}

func TestCandidateNodes_IntersectsHealthyAndTrunkNodeAddrs(t *testing.T) {
	t.Parallel()
	r := &Router{
		pool:   &fakePoolInternal{healthy: []string{"a", "b", "c"}},
		logger: zaptest.NewLogger(t),
	}
	trunk := Trunk{NodeAddrs: []string{"b", "c", "d"}}
	got := r.candidateNodes(trunk)
	require.Equal(t, []string{"b", "c"}, got, "intersection of trunk nodes and healthy nodes")
}

func TestCandidateNodes_EmptyHealthyReturnsNil(t *testing.T) {
	t.Parallel()
	r := &Router{
		pool:   &fakePoolInternal{healthy: nil},
		logger: zaptest.NewLogger(t),
	}
	got := r.candidateNodes(Trunk{NodeAddrs: []string{"a"}})
	require.Nil(t, got)
}

func TestCandidateNodes_NoIntersectionReturnsEmpty(t *testing.T) {
	t.Parallel()
	r := &Router{
		pool:   &fakePoolInternal{healthy: []string{"a", "b"}},
		logger: zaptest.NewLogger(t),
	}
	got := r.candidateNodes(Trunk{NodeAddrs: []string{"c", "d"}})
	require.Empty(t, got)
}

func TestStrategyByName_UnknownReturnsLeastCost(t *testing.T) {
	t.Parallel()
	r := &Router{
		strategies: map[string]Strategy{"least_cost": LeastCost{}},
	}
	got := r.strategyByName("totally-unknown")
	_, ok := got.(LeastCost)
	require.True(t, ok, "unknown strategy must fall back to LeastCost")
}

// TestRouter_ReleaseChannel_PropagatesRedisError exercises the wrap-and-
// return path: closing the miniredis makes every subsequent command fail,
// which Release converts into a wrapped error. This is the only way to
// reach the err branch of releaseScript.Run without faking the redis.Client.
func TestRouter_ReleaseChannel_PropagatesRedisError(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	r := &Router{
		bp:     NewBackpressure(rdb, 1),
		logger: zaptest.NewLogger(t),
	}
	mr.Close()
	err := r.ReleaseChannel(context.Background(), "n1")
	require.Error(t, err, "closed miniredis must propagate as a wrapped redis error")
}

func TestBackpressure_TryAcquire_PropagatesRedisError(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	bp := NewBackpressure(rdb, 5)
	mr.Close()
	_, err := bp.TryAcquire(context.Background(), "n1")
	require.Error(t, err)
}

func TestBackpressure_Get_PropagatesRedisError(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	bp := NewBackpressure(rdb, 5)
	mr.Close()
	_, err := bp.Get(context.Background(), "n1")
	require.Error(t, err)
}

func TestBackpressure_SetActiveChannels_PropagatesRedisError(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	bp := NewBackpressure(rdb, 5)
	mr.Close()
	err := bp.SetActiveChannels(context.Background(), "n1", 3)
	require.Error(t, err)
}

func TestBackpressure_Release_PropagatesRedisError(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	bp := NewBackpressure(rdb, 5)
	mr.Close()
	err := bp.Release(context.Background(), "n1")
	require.Error(t, err)
}

func TestRouter_Select_LogsAndContinuesOnTryAcquireError(t *testing.T) {
	t.Parallel()
	// Build a router whose Pool returns ["dead", "alive"]; the dead node
	// fails (closed redis on a different client per node? — too fragile).
	// Easier: use a Pool stub that returns one node where the redis is
	// closed; the loop logs+continues, and since there's only one node,
	// the result is "no node accepted backpressure". We cover the warn
	// log branch specifically.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	r := &Router{
		pool:   &fakePoolInternal{healthy: []string{"n1"}},
		bp:     NewBackpressure(rdb, 1),
		logger: zaptest.NewLogger(t),
		strategies: map[string]Strategy{
			"least_cost": LeastCost{},
		},
		defaultStrat: "least_cost",
		trunks: []Trunk{
			{ID: "primary", GatewayName: "gw-primary", Active: true, CostPerMin: 0.02},
		},
	}
	mr.Close()
	chosen, err := r.acquireFirstHealthy(context.Background(), []string{"n1"})
	require.NoError(t, err, "redis errors are logged + skipped, not returned")
	require.Empty(t, chosen, "no healthy node accepted")
}

// TestRouter_Stop_IsNoop covers the explicit Stop() branch — important so
// the composition root's defer rt.Stop() is exercised at least once in tests.
func TestRouter_Stop_IsNoop(t *testing.T) {
	t.Parallel()
	r := &Router{}
	r.Stop()
	r.Stop() // idempotent
}

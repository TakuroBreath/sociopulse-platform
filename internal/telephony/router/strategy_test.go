package router_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/telephony/router"
)

// LeastCost --------------------------------------------------------------

func TestLeastCost_PicksLowestCost(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "a", CostPerMin: 0.05, Active: true},
		{ID: "b", CostPerMin: 0.03, Active: true},
		{ID: "c", CostPerMin: 0.04, Active: true},
	}
	chosen, err := router.LeastCost{}.Pick(trunks, "+79991112233")
	require.NoError(t, err)
	require.Equal(t, "b", chosen.ID)
}

func TestLeastCost_SkipsInactive(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "a", CostPerMin: 0.05, Active: true},
		{ID: "b", CostPerMin: 0.03, Active: true},
		{ID: "c", CostPerMin: 0.02, Active: false}, // cheapest but inactive
	}
	chosen, err := router.LeastCost{}.Pick(trunks, "+79991112233")
	require.NoError(t, err)
	require.Equal(t, "b", chosen.ID, "inactive trunk c must be skipped even though it's cheapest")
}

func TestLeastCost_NoActiveReturnsErr(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "a", CostPerMin: 0.05, Active: false},
		{ID: "b", CostPerMin: 0.03, Active: false},
	}
	_, err := router.LeastCost{}.Pick(trunks, "+x")
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

func TestLeastCost_EmptySliceReturnsErr(t *testing.T) {
	t.Parallel()
	_, err := router.LeastCost{}.Pick(nil, "+x")
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

// RoundRobin -------------------------------------------------------------

func TestRoundRobin_DeterministicOrder(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		// Intentionally reverse-ordered: RoundRobin must sort by ID.
		{ID: "c", Active: true},
		{ID: "a", Active: true},
		{ID: "b", Active: true},
	}
	rr := &router.RoundRobin{}
	got := make([]string, 0, 6)
	for range 6 {
		c, err := rr.Pick(trunks, "+x")
		require.NoError(t, err)
		got = append(got, c.ID)
	}
	require.Equal(t, []string{"a", "b", "c", "a", "b", "c"}, got)
}

func TestRoundRobin_OnlyActiveTrunks(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "a", Active: true},
		{ID: "b", Active: false}, // skipped
		{ID: "c", Active: true},
	}
	rr := &router.RoundRobin{}
	got := make([]string, 0, 4)
	for range 4 {
		c, err := rr.Pick(trunks, "+x")
		require.NoError(t, err)
		got = append(got, c.ID)
	}
	require.Equal(t, []string{"a", "c", "a", "c"}, got, "inactive trunks must be filtered before round-robin")
}

func TestRoundRobin_NoActiveReturnsErr(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "a", Active: false},
		{ID: "b", Active: false},
	}
	rr := &router.RoundRobin{}
	_, err := rr.Pick(trunks, "+x")
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

func TestRoundRobin_Concurrent(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "a", Active: true},
		{ID: "b", Active: true},
		{ID: "c", Active: true},
	}
	rr := &router.RoundRobin{}

	const total = 1000
	counts := make(map[string]int, 3)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range total {
		wg.Go(func() {
			c, err := rr.Pick(trunks, "+x")
			if err != nil {
				return
			}
			mu.Lock()
			counts[c.ID]++
			mu.Unlock()
		})
	}
	wg.Wait()

	// 1000 / 3 ≈ 333; the atomic-counter contract guarantees an exact
	// even split modulo any remainder. With total=1000 and 3 trunks, two
	// IDs get 334 and one gets 332 (or any rotation — order depends on
	// goroutine scheduling but the counts are stable).
	sum := 0
	for _, n := range counts {
		sum += n
	}
	require.Equal(t, total, sum, "every Pick must record exactly one trunk")
	for id, n := range counts {
		assert.GreaterOrEqual(t, n, 332, "trunk %s got %d picks; expected ~333", id, n)
		assert.LessOrEqual(t, n, 335, "trunk %s got %d picks; expected ~333", id, n)
	}
}

// Weighted ---------------------------------------------------------------

func TestWeighted_DistributionApproximatesRatios(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "a", Weight: 70, Active: true},
		{ID: "b", Weight: 30, Active: true},
	}
	const iterations = 10000
	counts := map[string]int{}
	for range iterations {
		c, err := router.Weighted{}.Pick(trunks, "+x")
		require.NoError(t, err)
		counts[c.ID]++
	}
	require.InDelta(t, 0.7, float64(counts["a"])/iterations, 0.05, "trunk a should win ~70%% of picks")
	require.InDelta(t, 0.3, float64(counts["b"])/iterations, 0.05, "trunk b should win ~30%% of picks")
}

func TestWeighted_NoActiveReturnsErr(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "a", Weight: 70, Active: false},
		{ID: "b", Weight: 30, Active: false},
	}
	_, err := router.Weighted{}.Pick(trunks, "+x")
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

func TestWeighted_AllZeroWeightReturnsErr(t *testing.T) {
	t.Parallel()
	// Active but zero-weighted trunks contribute nothing to totalW; the
	// strategy treats this as "no eligible trunk".
	trunks := []router.Trunk{
		{ID: "a", Weight: 0, Active: true},
		{ID: "b", Weight: 0, Active: true},
	}
	_, err := router.Weighted{}.Pick(trunks, "+x")
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

func TestWeighted_SkipsInactiveAndZeroWeight(t *testing.T) {
	t.Parallel()
	// Only trunk "live" can win — the others are filtered out.
	trunks := []router.Trunk{
		{ID: "live", Weight: 10, Active: true},
		{ID: "off", Weight: 90, Active: false},
		{ID: "zero", Weight: 0, Active: true},
	}
	for range 100 {
		c, err := router.Weighted{}.Pick(trunks, "+x")
		require.NoError(t, err)
		require.Equal(t, "live", c.ID)
	}
}

// LeastCostWithFallback --------------------------------------------------

func TestLeastCostWithFallback_FiltersHighFailureRate(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "primary", CostPerMin: 0.02, Active: true, FailureRate: 0.6},
		{ID: "backup", CostPerMin: 0.05, Active: true, FailureRate: 0.05},
	}
	chosen, err := router.LeastCostWithFallback{FailureThreshold: 0.5}.Pick(trunks, "+x")
	require.NoError(t, err)
	require.Equal(t, "backup", chosen.ID, "primary's FailureRate=0.6 exceeds threshold 0.5")
}

func TestLeastCostWithFallback_FallsBackToFailingWhenAllExceed(t *testing.T) {
	t.Parallel()
	// Every trunk is over the threshold; the strategy must still pick the
	// cheapest rather than fail the call. Plan 09 references doc gotcha #6.
	trunks := []router.Trunk{
		{ID: "primary", CostPerMin: 0.02, Active: true, FailureRate: 0.9},
		{ID: "backup", CostPerMin: 0.05, Active: true, FailureRate: 0.8},
	}
	chosen, err := router.LeastCostWithFallback{FailureThreshold: 0.5}.Pick(trunks, "+x")
	require.NoError(t, err)
	require.Equal(t, "primary", chosen.ID, "fallback to LeastCost when every trunk exceeds threshold")
}

func TestLeastCostWithFallback_IgnoresInactive(t *testing.T) {
	t.Parallel()
	// Inactive trunks must not be picked even when they're under the
	// threshold and cheaper.
	trunks := []router.Trunk{
		{ID: "primary", CostPerMin: 0.02, Active: false, FailureRate: 0.0},
		{ID: "backup", CostPerMin: 0.05, Active: true, FailureRate: 0.0},
	}
	chosen, err := router.LeastCostWithFallback{FailureThreshold: 0.5}.Pick(trunks, "+x")
	require.NoError(t, err)
	require.Equal(t, "backup", chosen.ID)
}

func TestLeastCostWithFallback_NoActiveAtAllReturnsErr(t *testing.T) {
	t.Parallel()
	trunks := []router.Trunk{
		{ID: "primary", CostPerMin: 0.02, Active: false, FailureRate: 0.0},
	}
	_, err := router.LeastCostWithFallback{FailureThreshold: 0.5}.Pick(trunks, "+x")
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
}

package router_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/telephony/router"
)

// newBPClient wires a miniredis-backed *redis.Client and returns it together
// with the miniredis handle (so callers can FastForward the clock for TTL
// tests). The redis client is auto-closed via t.Cleanup so each test starts
// fresh.
func newBPClient(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func TestBackpressure_TryAcquireUnderCap(t *testing.T) {
	t.Parallel()
	rdb, _ := newBPClient(t)
	bp := router.NewBackpressure(rdb, 3)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		ok, err := bp.TryAcquire(ctx, "node-1")
		require.NoError(t, err)
		require.True(t, ok, "acquire %d/3 should succeed (cap=3)", i+1)
	}
}

func TestBackpressure_RejectsAtCap(t *testing.T) {
	t.Parallel()
	rdb, _ := newBPClient(t)
	bp := router.NewBackpressure(rdb, 60)
	ctx := context.Background()

	for i := 0; i < 60; i++ {
		ok, err := bp.TryAcquire(ctx, "node-1")
		require.NoError(t, err)
		require.True(t, ok, "acquire %d/60 should succeed", i+1)
	}
	// 61st must be rejected.
	ok, err := bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.False(t, ok, "acquire 61 should be rejected at cap=60")

	// Release one and try again — should succeed.
	require.NoError(t, bp.Release(ctx, "node-1"))
	ok, err = bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.True(t, ok, "after Release, one slot should be free")
}

func TestBackpressure_PerNodeCounters(t *testing.T) {
	t.Parallel()
	rdb, _ := newBPClient(t)
	bp := router.NewBackpressure(rdb, 2)
	ctx := context.Background()

	// node-1: fill to cap.
	ok, err := bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.False(t, ok, "node-1 at cap")

	// node-2 has its own counter — must accept.
	ok, err = bp.TryAcquire(ctx, "node-2")
	require.NoError(t, err)
	require.True(t, ok, "node-2 counter is independent of node-1")

	got1, err := bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 2, got1)
	got2, err := bp.Get(ctx, "node-2")
	require.NoError(t, err)
	require.Equal(t, 1, got2)
}

func TestBackpressure_Release_Decrements(t *testing.T) {
	t.Parallel()
	rdb, _ := newBPClient(t)
	bp := router.NewBackpressure(rdb, 5)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		ok, err := bp.TryAcquire(ctx, "node-1")
		require.NoError(t, err)
		require.True(t, ok)
	}
	require.NoError(t, bp.Release(ctx, "node-1"))
	got, err := bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 2, got, "counter should drop from 3 to 2 after one Release")
}

func TestBackpressure_Release_StaysAtZero(t *testing.T) {
	t.Parallel()
	rdb, _ := newBPClient(t)
	bp := router.NewBackpressure(rdb, 5)
	ctx := context.Background()

	// Release without prior Acquire — must be a no-op.
	require.NoError(t, bp.Release(ctx, "node-1"))
	got, err := bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 0, got, "Release on missing key must not produce a negative counter")

	// Acquire once, release twice — counter should not go negative.
	ok, err := bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, bp.Release(ctx, "node-1"))
	require.NoError(t, bp.Release(ctx, "node-1"))
	got, err = bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 0, got, "double-release must clamp at 0, not -1")
}

func TestBackpressure_Get_ReturnsZeroWhenMissing(t *testing.T) {
	t.Parallel()
	rdb, _ := newBPClient(t)
	bp := router.NewBackpressure(rdb, 5)
	ctx := context.Background()

	got, err := bp.Get(ctx, "missing-node")
	require.NoError(t, err)
	require.Equal(t, 0, got)
}

func TestBackpressure_SetActiveChannels_Overwrites(t *testing.T) {
	t.Parallel()
	rdb, _ := newBPClient(t)
	bp := router.NewBackpressure(rdb, 60)
	ctx := context.Background()

	// Start with 5 acquires; counter = 5.
	for i := 0; i < 5; i++ {
		ok, err := bp.TryAcquire(ctx, "node-1")
		require.NoError(t, err)
		require.True(t, ok)
	}
	got, err := bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 5, got)

	// Reconciler observes that FS actually has 12 channels in use; sync.
	require.NoError(t, bp.SetActiveChannels(ctx, "node-1", 12))
	got, err = bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 12, got)

	// Negative input clamps to 0.
	require.NoError(t, bp.SetActiveChannels(ctx, "node-1", -3))
	got, err = bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 0, got)
}

func TestBackpressure_TTLRefreshedOnAcquire(t *testing.T) {
	t.Parallel()
	rdb, mr := newBPClient(t)
	bp := router.NewBackpressure(rdb, 5)
	ctx := context.Background()

	// Acquire — sets TTL to 1h.
	ok, err := bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.True(t, ok)

	// Advance miniredis clock by 30 minutes; key should still exist.
	mr.FastForward(30 * time.Minute)
	got, err := bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 1, got, "key must still be present 30 min in")

	// Acquire again — refreshes TTL.
	ok, err = bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.True(t, ok)

	// Advance another 45 min — without the refresh, total elapsed (75 min)
	// would exceed the 1h TTL. With refresh, the key persists.
	mr.FastForward(45 * time.Minute)
	got, err = bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 2, got, "TTL refresh on second Acquire should keep the key alive past the original 1h horizon")
}

func TestBackpressure_TTLExpiresWithoutRefresh(t *testing.T) {
	t.Parallel()
	rdb, mr := newBPClient(t)
	bp := router.NewBackpressure(rdb, 5)
	ctx := context.Background()

	ok, err := bp.TryAcquire(ctx, "node-1")
	require.NoError(t, err)
	require.True(t, ok)

	// Advance past 1h — key must expire.
	mr.FastForward(2 * time.Hour)
	got, err := bp.Get(ctx, "node-1")
	require.NoError(t, err)
	require.Equal(t, 0, got, "key should have expired after 2h with no refresh")
}

func TestBackpressure_NewBackpressure_DefaultsCap(t *testing.T) {
	t.Parallel()
	rdb, _ := newBPClient(t)
	bp := router.NewBackpressure(rdb, 0) // 0 → default 60
	require.Equal(t, 60, bp.Cap())

	bp = router.NewBackpressure(rdb, -10) // negative → default 60
	require.Equal(t, 60, bp.Cap())

	bp = router.NewBackpressure(rdb, 42) // explicit cap honored
	require.Equal(t, 42, bp.Cap())
}

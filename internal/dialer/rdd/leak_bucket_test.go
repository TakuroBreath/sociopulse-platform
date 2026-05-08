package rdd

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// bucketFixture wires a miniredis-backed LeakBucket with a frozen
// clock the tests advance explicitly. miniredis interprets the Lua
// script's HMSET / HMGET / EXPIRE semantics correctly for our use; the
// integration test exercises real Redis 7.4.
type bucketFixture struct {
	mr    *miniredis.Miniredis
	rdb   *redis.Client
	clock *fakeClock
	b     *LeakBucket
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newBucketFixture(t *testing.T, rate int) *bucketFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	b := newLeakBucket(rdb, rate, time.Hour, clk.Now)
	return &bucketFixture{mr: mr, rdb: rdb, clock: clk, b: b}
}

// TestLeakBucket_FullBucket — a fresh bucket allows up to capacity
// requests in succession, then throttles. The clock does not advance
// so no refill happens.
func TestLeakBucket_FullBucket(t *testing.T) {
	t.Parallel()
	const capacity = 5
	f := newBucketFixture(t, capacity)
	tenantID := uuid.New()
	ctx := context.Background()

	// First `capacity` calls must all succeed.
	for i := range capacity {
		ok, err := f.b.Allow(ctx, tenantID)
		require.NoError(t, err, "iteration %d", i)
		require.True(t, ok, "iteration %d must consume token", i)
	}

	// Next call exhausts the bucket → throttled.
	ok, err := f.b.Allow(ctx, tenantID)
	require.NoError(t, err)
	require.False(t, ok, "bucket must throttle after capacity reached")
}

// TestLeakBucket_RefillOverTime — advancing the clock by one second
// at a rate=10 bucket replenishes ~10 tokens (capped at capacity).
func TestLeakBucket_RefillOverTime(t *testing.T) {
	t.Parallel()
	const rate = 10
	f := newBucketFixture(t, rate)
	tenantID := uuid.New()
	ctx := context.Background()

	// Drain the bucket.
	for range rate {
		ok, err := f.b.Allow(ctx, tenantID)
		require.NoError(t, err)
		require.True(t, ok)
	}
	// Confirm throttle.
	ok, err := f.b.Allow(ctx, tenantID)
	require.NoError(t, err)
	require.False(t, ok)

	// Advance one full second — bucket should refill back to capacity.
	f.clock.now = f.clock.now.Add(time.Second)
	for i := range rate {
		ok, err := f.b.Allow(ctx, tenantID)
		require.NoError(t, err, "post-refill iteration %d", i)
		require.True(t, ok, "post-refill iteration %d must consume token", i)
	}
	// Bucket should be exhausted again.
	ok, err = f.b.Allow(ctx, tenantID)
	require.NoError(t, err)
	require.False(t, ok)
}

// TestLeakBucket_PartialRefill — half a second of elapsed time at
// rate=10 yields ~5 tokens. The exact integer threshold matters since
// we accept fractional accumulation but only consume whole tokens.
func TestLeakBucket_PartialRefill(t *testing.T) {
	t.Parallel()
	const rate = 10
	f := newBucketFixture(t, rate)
	tenantID := uuid.New()
	ctx := context.Background()

	// Drain.
	for range rate {
		ok, err := f.b.Allow(ctx, tenantID)
		require.NoError(t, err)
		require.True(t, ok)
	}
	// 500ms later: ~5 tokens (rate * 0.5).
	f.clock.now = f.clock.now.Add(500 * time.Millisecond)
	allowed := 0
	for range rate { // try to consume up to `rate` again
		ok, err := f.b.Allow(ctx, tenantID)
		require.NoError(t, err)
		if ok {
			allowed++
		}
	}
	require.GreaterOrEqual(t, allowed, 4, "partial refill should yield at least 4 tokens")
	require.LessOrEqual(t, allowed, 6, "partial refill should yield at most ~6 tokens (allowing for fractional rounding)")
}

// TestLeakBucket_PerTenantIsolation — two tenants share a Redis
// instance but their buckets are independent.
func TestLeakBucket_PerTenantIsolation(t *testing.T) {
	t.Parallel()
	const rate = 3
	f := newBucketFixture(t, rate)
	tenantA, tenantB := uuid.New(), uuid.New()
	ctx := context.Background()

	// Drain tenant A.
	for range rate {
		ok, err := f.b.Allow(ctx, tenantA)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := f.b.Allow(ctx, tenantA)
	require.NoError(t, err)
	require.False(t, ok, "tenant A must be throttled after draining")

	// Tenant B is untouched.
	for range rate {
		ok, err := f.b.Allow(ctx, tenantB)
		require.NoError(t, err)
		require.True(t, ok, "tenant B should not be affected by tenant A's exhaustion")
	}
}

// TestLeakBucket_TTLRefresh — every Allow refreshes the bucket key
// TTL so an active tenant's bucket never silently disappears.
func TestLeakBucket_TTLRefresh(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	const ttl = 30 * time.Minute
	b := newLeakBucket(rdb, 5, ttl, clk.Now)
	tenantID := uuid.New()
	ctx := context.Background()

	_, err := b.Allow(ctx, tenantID)
	require.NoError(t, err)
	require.Equal(t, ttl, mr.TTL(b.key(tenantID)))
}

// TestLeakBucket_RedisFailureIsErr — closing the client mid-Allow
// surfaces a transport error to the caller.
func TestLeakBucket_RedisFailureIsErr(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	b := newLeakBucket(rdb, 5, time.Hour, clk.Now)

	require.NoError(t, rdb.Close())

	ok, err := b.Allow(context.Background(), uuid.New())
	require.Error(t, err)
	require.False(t, ok)
	require.Contains(t, err.Error(), "leakbucket")
}

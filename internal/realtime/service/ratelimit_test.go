package service

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTokenBucket_AllowsBurst(t *testing.T) {
	t.Parallel()

	clock := &controllableClock{now: time.Unix(0, 0)}
	b := newTokenBucket(10, 5, clock.Now)

	// Burst capacity 5: first 5 calls succeed, 6th fails.
	for i := range 5 {
		require.True(t, b.Allow(), "call %d expected within burst", i)
	}
	require.False(t, b.Allow(), "6th call must be denied (burst exhausted)")
}

func TestTokenBucket_Refills(t *testing.T) {
	t.Parallel()

	clock := &controllableClock{now: time.Unix(0, 0)}
	b := newTokenBucket(10, 5, clock.Now)

	// Drain the bucket.
	for range 5 {
		require.True(t, b.Allow())
	}
	require.False(t, b.Allow())

	// Advance 200ms — at 10 tokens/sec that's exactly 2 tokens.
	clock.Bump(200 * time.Millisecond)
	require.True(t, b.Allow())
	require.True(t, b.Allow())
	require.False(t, b.Allow(), "3rd call after 2-token refill must be denied")
}

func TestTokenBucket_RefillCapsAtBurst(t *testing.T) {
	t.Parallel()

	clock := &controllableClock{now: time.Unix(0, 0)}
	b := newTokenBucket(10, 5, clock.Now)

	// Drain.
	for range 5 {
		require.True(t, b.Allow())
	}

	// Advance 1 minute — enough refill to overflow burst many
	// times. Bucket must cap at burst (5).
	clock.Bump(time.Minute)
	for i := range 5 {
		require.True(t, b.Allow(), "call %d expected within refilled burst", i)
	}
	require.False(t, b.Allow(), "must not exceed burst even after long idle")
}

func TestTokenBucket_UnlimitedRate(t *testing.T) {
	t.Parallel()

	clock := &controllableClock{now: time.Unix(0, 0)}
	b := newTokenBucket(0, 0, clock.Now)

	for range 1000 {
		require.True(t, b.Allow(), "rate=0 must be unlimited")
	}
}

func TestTokenBucket_NilSafe(t *testing.T) {
	t.Parallel()

	var b *tokenBucket
	require.True(t, b.Allow(), "nil bucket must allow (no-op limiter)")
}

func TestTokenBucket_ConcurrentSafe(t *testing.T) {
	t.Parallel()

	clock := &controllableClock{now: time.Unix(0, 0)}
	b := newTokenBucket(1000, 1000, clock.Now)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 10 {
				_ = b.Allow()
			}
		})
	}
	wg.Wait()
	// 500 calls against 1000-burst budget — all must have
	// succeeded. Non-flaky: bucket starts full.
	require.GreaterOrEqual(t, b.Tokens(), 500.0,
		"expected ≥500 tokens left after 500 calls against 1000 burst")
}

func TestTokenBucket_BurstDefaultsToRate(t *testing.T) {
	t.Parallel()

	clock := &controllableClock{now: time.Unix(0, 0)}
	b := newTokenBucket(3, 0, clock.Now)

	for range 3 {
		require.True(t, b.Allow())
	}
	require.False(t, b.Allow())
}

func TestTokenBucket_NilClockUsesTimeNow(t *testing.T) {
	t.Parallel()

	b := newTokenBucket(100, 100, nil)
	require.NotNil(t, b)
	// First call must succeed — bucket starts full.
	require.True(t, b.Allow())
}

// controllableClock is a test-local controllable clock.
type controllableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *controllableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controllableClock) Bump(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

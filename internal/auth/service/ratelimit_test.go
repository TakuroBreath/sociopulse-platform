package service_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/auth/service"
)

// newRateLimiterT constructs a RateLimiterRedis bound to a fresh miniredis.
// The miniredis is auto-closed via t.Cleanup; the redis client is closed
// explicitly so goleak doesn't flag the network-pool worker. window is
// fixed at 1 hour — every test uses the production window so the
// rolling-window arithmetic is exercised against the spec target.
func newRateLimiterT(
	t *testing.T,
	perIP, perUser int,
	clock func() time.Time,
) (*service.RateLimiterRedis, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	rl := service.NewRateLimiterRedis(rdb, perIP, perUser, time.Hour, clock)
	return rl, mr
}

func mustParseIP(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	require.NoError(t, err)
	return a
}

// 1. After perIPPerHour hits in the same window, AllowIP returns false; subsequent calls also false.
func TestRateLimiter_AllowIP_TripsAtMaximum(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	rl, _ := newRateLimiterT(t, 30, 10, clock)
	ctx := t.Context()

	ip := mustParseIP(t, "10.0.0.1")

	// First 30 calls allowed.
	for i := 0; i < 30; i++ {
		ok, err := rl.AllowIP(ctx, ip)
		require.NoError(t, err)
		assert.True(t, ok, "expected hit %d to be allowed", i+1)
	}

	// 31st call rejected.
	ok, err := rl.AllowIP(ctx, ip)
	require.NoError(t, err)
	assert.False(t, ok, "expected 31st hit to be rejected")

	// 32nd call also rejected (every attempt counts).
	ok, err = rl.AllowIP(ctx, ip)
	require.NoError(t, err)
	assert.False(t, ok, "expected 32nd hit to be rejected")
}

// 2. After window+1s passes (miniredis.FastForward), the bucket releases.
func TestRateLimiter_AllowIP_BucketReleasesAfterWindow(t *testing.T) {
	t.Parallel()

	current := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return current }

	rl, mr := newRateLimiterT(t, 3, 10, clock)
	ctx := t.Context()

	ip := mustParseIP(t, "10.0.0.2")

	// Saturate the bucket.
	for i := 0; i < 3; i++ {
		ok, err := rl.AllowIP(ctx, ip)
		require.NoError(t, err)
		assert.True(t, ok)
	}
	ok, err := rl.AllowIP(ctx, ip)
	require.NoError(t, err)
	assert.False(t, ok, "bucket should be saturated")

	// Advance both wall-clock and miniredis clock past the window so old
	// entries are pruned by ZRemRangeByScore on the next call.
	current = current.Add(time.Hour + time.Second)
	mr.FastForward(time.Hour + time.Second)

	ok, err = rl.AllowIP(ctx, ip)
	require.NoError(t, err)
	assert.True(t, ok, "bucket should release after window")
}

// 3. Two different IPs are independent.
func TestRateLimiter_AllowIP_PerIPIndependence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	rl, _ := newRateLimiterT(t, 2, 10, clock)
	ctx := t.Context()

	ipA := mustParseIP(t, "10.0.0.10")
	ipB := mustParseIP(t, "10.0.0.11")

	// Saturate IP A.
	for i := 0; i < 2; i++ {
		ok, err := rl.AllowIP(ctx, ipA)
		require.NoError(t, err)
		assert.True(t, ok)
	}
	ok, err := rl.AllowIP(ctx, ipA)
	require.NoError(t, err)
	assert.False(t, ok, "ipA should be saturated")

	// IP B is unaffected.
	ok, err = rl.AllowIP(ctx, ipB)
	require.NoError(t, err)
	assert.True(t, ok, "ipB should be unaffected by ipA saturation")
}

// 4. Per-account limit (10/h) trips at 10 even if IP limit (30) hasn't.
func TestRateLimiter_AllowAccount_TripsBeforeIPLimit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	rl, _ := newRateLimiterT(t, 30, 10, clock)
	ctx := t.Context()

	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	for i := 0; i < 10; i++ {
		ok, err := rl.AllowAccount(ctx, uid)
		require.NoError(t, err)
		assert.True(t, ok, "expected account hit %d to be allowed", i+1)
	}

	ok, err := rl.AllowAccount(ctx, uid)
	require.NoError(t, err)
	assert.False(t, ok, "11th account hit should be rejected")
}

// 5. Zero netip.Addr does not panic — graceful degradation.
func TestRateLimiter_AllowIP_ZeroAddrIsSafe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	rl, _ := newRateLimiterT(t, 5, 10, clock)
	ctx := t.Context()

	// Default-constructed netip.Addr is the invalid sentinel.
	var zero netip.Addr
	ok, err := rl.AllowIP(ctx, zero)
	require.NoError(t, err)
	assert.True(t, ok)
}

// 6. Pipeline Exec error surfaces from AllowIP (use a closed miniredis).
func TestRateLimiter_AllowIP_PipelineErrorPropagates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	rl, mr := newRateLimiterT(t, 5, 10, clock)
	ctx := t.Context()

	// Close miniredis so subsequent commands fail.
	mr.Close()

	ip := mustParseIP(t, "10.0.0.99")
	ok, err := rl.AllowIP(ctx, ip)
	require.Error(t, err, "expected error from closed redis")
	assert.False(t, ok)
}

// 7. Two independent users share the IP — IP counter increments for both,
// account counters are separate. Verifies key-derivation independence.
func TestRateLimiter_PerIP_PerAccount_KeySpaceIndependence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	rl, _ := newRateLimiterT(t, 4, 2, clock)
	ctx := t.Context()

	ip := mustParseIP(t, "10.0.0.50")
	uA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	uB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	// User A: two account hits + IP hits.
	for i := 0; i < 2; i++ {
		ok, err := rl.AllowAccount(ctx, uA)
		require.NoError(t, err)
		assert.True(t, ok)
		ok, err = rl.AllowIP(ctx, ip)
		require.NoError(t, err)
		assert.True(t, ok)
	}

	// User A's account is saturated; user B's account is fresh.
	ok, err := rl.AllowAccount(ctx, uA)
	require.NoError(t, err)
	assert.False(t, ok, "user A account should be saturated")

	ok, err = rl.AllowAccount(ctx, uB)
	require.NoError(t, err)
	assert.True(t, ok, "user B account should be fresh despite shared IP")

	// IP allowed two more (we used 2 of 4); third call (the user-B account
	// allow above did NOT also increment IP) — IP counter sees only the
	// 2 explicit AllowIP calls; another 2 are still allowed.
	for i := 0; i < 2; i++ {
		ok, err = rl.AllowIP(ctx, ip)
		require.NoError(t, err)
		assert.True(t, ok)
	}
	ok, err = rl.AllowIP(ctx, ip)
	require.NoError(t, err)
	assert.False(t, ok, "5th IP hit should be rejected")
}

// 8. Constructor zero-defaults: nil clock -> time.Now; window=0 -> 1h;
// limits 0 -> sensible defaults (30 IP, 10 account).
func TestRateLimiter_Defaults(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// All zeros — verify the constructor fills in sensible defaults.
	rl := service.NewRateLimiterRedis(rdb, 0, 0, 0, nil)
	require.NotNil(t, rl)

	ctx := context.Background()
	ip := mustParseIP(t, "10.0.0.123")

	// 30 hits should pass with default IP limit.
	for i := 0; i < 30; i++ {
		ok, err := rl.AllowIP(ctx, ip)
		require.NoError(t, err)
		assert.True(t, ok, "default IP limit should allow 30 hits, failed at %d", i+1)
	}

	// 31st should reject under default IP limit.
	ok, err := rl.AllowIP(ctx, ip)
	require.NoError(t, err)
	assert.False(t, ok)
}

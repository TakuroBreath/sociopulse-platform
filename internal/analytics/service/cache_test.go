package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/analytics/service"
)

// newMiniRedis spins up an in-memory miniredis, returns a connected
// go-redis client + the FakeTime-driven server handle, and reaps both
// on t.Cleanup. Used by every cache_test.go case.
func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// TestRedisCache_GetMissReturnsFalse asserts that GET on an unknown
// key returns (nil, false, nil) — the redis.Nil sentinel must be
// translated into the cache-miss shape, NOT propagated as an error.
func TestRedisCache_GetMissReturnsFalse(t *testing.T) {
	t.Parallel()
	_, rdb := newMiniRedis(t)
	cache := service.NewRedisCache(rdb, zap.NewNop())

	raw, hit, err := cache.Get(t.Context(), "missing-key")
	require.NoError(t, err)
	require.False(t, hit)
	require.Nil(t, raw)
}

// TestRedisCache_SetThenGetRoundTrip writes a value with Set, reads it
// back with Get, and asserts byte-equality. Exercises the gzip codec
// round-trip end-to-end.
func TestRedisCache_SetThenGetRoundTrip(t *testing.T) {
	t.Parallel()
	_, rdb := newMiniRedis(t)
	cache := service.NewRedisCache(rdb, zap.NewNop())

	want := []byte(`{"hello":"world","n":42}`)
	require.NoError(t, cache.Set(t.Context(), "k", want, 30*time.Second))

	got, hit, err := cache.Get(t.Context(), "k")
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, want, got)
}

// TestRedisCache_NilReceiver_NilClient_NoPanic exercises the nil-safety
// invariant — a degraded boot (no Redis wired) should return clean
// cache-miss shape instead of panicking.
func TestRedisCache_NilReceiver_NilClient_NoPanic(t *testing.T) {
	t.Parallel()

	t.Run("nil receiver", func(t *testing.T) {
		t.Parallel()
		var cache *service.RedisCache
		raw, hit, err := cache.Get(t.Context(), "k")
		require.NoError(t, err)
		require.False(t, hit)
		require.Nil(t, raw)
		require.NoError(t, cache.Set(t.Context(), "k", []byte("v"), time.Second))
	})

	t.Run("nil client", func(t *testing.T) {
		t.Parallel()
		cache := service.NewRedisCache(nil, zap.NewNop())
		raw, hit, err := cache.Get(t.Context(), "k")
		require.NoError(t, err)
		require.False(t, hit)
		require.Nil(t, raw)
		require.NoError(t, cache.Set(t.Context(), "k", []byte("v"), time.Second))
	})
}

// TestRedisCache_TTL writes with a 100ms TTL, advances miniredis's
// fake clock past expiry, then asserts the key has expired.
func TestRedisCache_TTL(t *testing.T) {
	t.Parallel()
	mr, rdb := newMiniRedis(t)
	cache := service.NewRedisCache(rdb, zap.NewNop())

	require.NoError(t, cache.Set(t.Context(), "k", []byte("payload"), 100*time.Millisecond))

	// Sanity: read back immediately.
	_, hit, err := cache.Get(t.Context(), "k")
	require.NoError(t, err)
	require.True(t, hit)

	mr.FastForward(200 * time.Millisecond)

	raw, hit, err := cache.Get(t.Context(), "k")
	require.NoError(t, err)
	require.False(t, hit, "expired key should miss")
	require.Nil(t, raw)
}

// TestRedisCache_GzipShrinksRepetitivePayload is a sanity check on the
// gzip codec — a value with high repetition should be smaller in
// Redis than its raw form. Catches accidental codec removals.
func TestRedisCache_GzipShrinksRepetitivePayload(t *testing.T) {
	t.Parallel()
	mr, rdb := newMiniRedis(t)
	cache := service.NewRedisCache(rdb, zap.NewNop())

	// 2 KiB of repetition compresses heavily under gzip.
	want := []byte(strings.Repeat("sociopulse-analytics-", 200))
	require.NoError(t, cache.Set(t.Context(), "k", want, time.Minute))

	stored, err := mr.Get("k")
	require.NoError(t, err)
	require.Less(t, len(stored), len(want), "gzip should shrink repetitive content")

	// And the round-trip still produces the original bytes.
	got, hit, err := cache.Get(t.Context(), "k")
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, want, got)
}

package nats_bridge_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/telephony/nats_bridge"
)

// TestIdempotency_FirstMarkSeenReturnsTrue asserts the happy path: a brand-
// new commandID returns (true, nil) — the caller should DISPATCH.
func TestIdempotency_FirstMarkSeenReturnsTrue(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	guard := nats_bridge.NewIdempotencyGuard(rdb, 24*time.Hour, zap.NewNop())

	first, err := guard.MarkSeen(context.Background(), "command-uuid-1")
	require.NoError(t, err)
	require.True(t, first, "first MarkSeen returns true (newly seen)")
}

// TestIdempotency_DuplicateWithinTTLReturnsFalse covers the dedup contract:
// the second call within TTL returns (false, nil) — the caller should ACK
// without redispatching. NACK on a duplicate would loop forever.
func TestIdempotency_DuplicateWithinTTLReturnsFalse(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	guard := nats_bridge.NewIdempotencyGuard(rdb, 24*time.Hour, zap.NewNop())

	first, err := guard.MarkSeen(context.Background(), "command-uuid-2")
	require.NoError(t, err)
	require.True(t, first)

	second, err := guard.MarkSeen(context.Background(), "command-uuid-2")
	require.NoError(t, err)
	require.False(t, second, "duplicate within TTL returns false")
}

// TestIdempotency_AcceptsAfterTTLExpires ensures the SETNX TTL is honoured:
// once Redis has GC'd the key, the same commandID is treated as new again.
// FastForward simulates wall-clock advance without sleeping the test.
func TestIdempotency_AcceptsAfterTTLExpires(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ttl := 24 * time.Hour
	guard := nats_bridge.NewIdempotencyGuard(rdb, ttl, zap.NewNop())

	first, err := guard.MarkSeen(context.Background(), "command-uuid-3")
	require.NoError(t, err)
	require.True(t, first)

	// Advance miniredis past the TTL window so the key expires.
	mr.FastForward(ttl + time.Second)

	again, err := guard.MarkSeen(context.Background(), "command-uuid-3")
	require.NoError(t, err)
	require.True(t, again, "post-TTL same id is accepted again")
}

// TestIdempotency_RedisFailureBubblesUp is the defence-against-silent-double-
// execution case: when Redis itself is unreachable, MarkSeen MUST return an
// error so the caller NACKs and the broker redelivers later. Returning
// (false, nil) silently here would let the command be dropped on the floor.
func TestIdempotency_RedisFailureBubblesUp(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	guard := nats_bridge.NewIdempotencyGuard(rdb, 24*time.Hour, zap.NewNop())

	mr.Close()

	_, err := guard.MarkSeen(context.Background(), "command-uuid-4")
	require.Error(t, err, "Redis failure must bubble up so caller NACKs")
}

// TestIdempotency_NewIdempotencyGuard_PanicsOnNilRedis hardens the
// constructor: a nil rdb is a wiring bug and panics at boot rather than at
// first MarkSeen call. Mirrors the pool/router metric-registry rule.
func TestIdempotency_NewIdempotencyGuard_PanicsOnNilRedis(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		_ = nats_bridge.NewIdempotencyGuard(nil, time.Hour, zap.NewNop())
	}, "nil Redis client must panic at construction")
}

// TestIdempotency_NewIdempotencyGuard_DefaultsZeroTTLTo24h asserts the
// default TTL contract documented on the constructor.
func TestIdempotency_NewIdempotencyGuard_DefaultsZeroTTLTo24h(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	guard := nats_bridge.NewIdempotencyGuard(rdb, 0, zap.NewNop())
	first, err := guard.MarkSeen(context.Background(), "command-uuid-5")
	require.NoError(t, err)
	require.True(t, first)

	// Advance just under 24h: still deduped.
	mr.FastForward(23 * time.Hour)
	again, err := guard.MarkSeen(context.Background(), "command-uuid-5")
	require.NoError(t, err)
	assert.False(t, again, "default TTL of 24h still dedups at 23h")
}

// TestIdempotency_NewIdempotencyGuard_NilLoggerOK proves the logger is
// nil-tolerated.
func TestIdempotency_NewIdempotencyGuard_NilLoggerOK(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	require.NotPanics(t, func() {
		guard := nats_bridge.NewIdempotencyGuard(rdb, time.Hour, nil)
		_, _ = guard.MarkSeen(context.Background(), "x")
	})
}

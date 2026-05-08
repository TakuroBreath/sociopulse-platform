package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Backpressure tracks the per-FS-node "active channels" counter via Redis,
// gating originates so a single FS node never exceeds the configured cap. The
// counter is incremented atomically on TryAcquire (Lua-bounded by cap) and
// decremented on Release (Lua-floored at 0). Plan 09 Task 6's reconciler
// uses SetActiveChannels to overwrite the counter when FS reports a different
// channels-in-use value — this is the eventual-consistency safety valve.
//
// Key format: op:active_channels:{node} — Plan 10's dialer reads the same
// keys (Get) for backoff decisions; the prefix is stable across plans.
type Backpressure struct {
	rdb *redis.Client
	cap int
	ttl time.Duration
}

// defaultBackpressureCap is the per-node cap used when the caller passes
// cap <= 0. Sourced from cfg.Telephony.Bridge.MaxConcurrentPerNode in
// production; the constant lives here so unit tests don't need a config
// snapshot.
const defaultBackpressureCap = 60

// defaultBackpressureTTL is the per-key TTL refreshed on every TryAcquire.
// One hour is long enough that a healthy node never sees expiry mid-call (no
// human-grade survey lasts an hour) but short enough that a permanently-
// failed FS node's stale counter clears itself rather than accumulating
// forever. The reconciler (Plan 09 Task 6) overwrites the counter directly
// when it observes drift, so TTL-driven cleanup is a backstop, not the
// primary correctness mechanism.
const defaultBackpressureTTL = time.Hour

// tryAcquireScript atomically increments op:active_channels:{node} iff the
// current value is below cap (ARGV[1]). On success, refreshes the TTL
// (ARGV[2] seconds) so a permanent FS-node failure eventually cleans up.
//
// Lua atomicity: Redis serializes script execution per Redis instance, so
// the GET / compare / INCR sequence cannot race with another TryAcquire on
// the same key. This is the contract Plan 09 references doc §"Redis Lua
// scripts" relies on — single-key scripts work on Redis Cluster too because
// every operation hashes to the same slot.
var tryAcquireScript = redis.NewScript(`
local v = tonumber(redis.call("GET", KEYS[1]) or "0")
if v >= tonumber(ARGV[1]) then
  return 0
end
redis.call("INCR", KEYS[1])
redis.call("EXPIRE", KEYS[1], ARGV[2])
return 1
`)

// releaseScript atomically decrements op:active_channels:{node} but never
// drops below zero. Idempotent on overshoot: a duplicate Release (e.g.
// CHANNEL_HANGUP_COMPLETE delivered twice through NATS redelivery) leaves
// the counter at 0 instead of going negative.
//
// EXPIRE refresh: keep the TTL alive while the counter is non-zero so a
// node still serving calls doesn't drop its key out from under
// active acquires. When the counter goes to 0 we still bump the TTL so an
// observer that polls for "is this node idle" sees the key for the same
// 1-hour window.
var releaseScript = redis.NewScript(`
local v = tonumber(redis.call("GET", KEYS[1]) or "0")
if v <= 0 then
  return 0
end
redis.call("DECR", KEYS[1])
redis.call("EXPIRE", KEYS[1], ARGV[1])
return 1
`)

// NewBackpressure constructs a Backpressure with the supplied cap. cap <= 0
// falls back to defaultBackpressureCap (60) so a misconfigured zero in Helm
// values does not silently disable the gate. rdb may be nil only in tests
// that never call any method; production callers must pass a connected
// client.
func NewBackpressure(rdb *redis.Client, cap int) *Backpressure {
	if cap <= 0 {
		cap = defaultBackpressureCap
	}
	return &Backpressure{
		rdb: rdb,
		cap: cap,
		ttl: defaultBackpressureTTL,
	}
}

// Cap returns the configured cap. Used by tests and metrics.
func (b *Backpressure) Cap() int { return b.cap }

// key formats op:active_channels:<node>. Stable across plans — Plan 10's
// dialer and Plan 09 Task 6's reconciler read the same prefix.
func (b *Backpressure) key(node string) string {
	return fmt.Sprintf("op:active_channels:%s", node)
}

// TryAcquire atomically reserves one channel slot on node. Returns true and
// (nil error) when the slot was claimed; false and (nil error) when the
// node is at cap; an error wrapping the underlying redis failure on a
// transport-level fault.
//
// Side effects on success: counter +1 and TTL refreshed to ttl. On failure
// (cap reached), the counter and TTL are unchanged.
func (b *Backpressure) TryAcquire(ctx context.Context, node string) (bool, error) {
	res, err := tryAcquireScript.Run(
		ctx, b.rdb,
		[]string{b.key(node)},
		b.cap, int(b.ttl.Seconds()),
	).Int()
	if err != nil {
		return false, fmt.Errorf("router/backpressure: try acquire %s: %w", node, err)
	}
	return res == 1, nil
}

// Release atomically returns one channel slot to node. Idempotent: calling
// Release when the counter is already 0 is a no-op (no negative values).
// Returns an error wrapping the underlying redis failure on transport-level
// faults. The "couldn't decrement, already 0" case is NOT an error — the
// caller (CHANNEL_HANGUP_COMPLETE handler) cannot tell if the prior INCR
// happened on a different bridge instance, so over-release is the expected
// degradation mode.
func (b *Backpressure) Release(ctx context.Context, node string) error {
	if _, err := releaseScript.Run(
		ctx, b.rdb,
		[]string{b.key(node)},
		int(b.ttl.Seconds()),
	).Int(); err != nil {
		return fmt.Errorf("router/backpressure: release %s: %w", node, err)
	}
	return nil
}

// Get returns the current counter value (0 if the key is absent). Used by
// the reconciler (Plan 09 Task 6) and by metrics polling. Errors only on
// transport-level redis failures.
func (b *Backpressure) Get(ctx context.Context, node string) (int, error) {
	v, err := b.rdb.Get(ctx, b.key(node)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("router/backpressure: get %s: %w", node, err)
	}
	return v, nil
}

// SetActiveChannels overwrites the counter to n. Used by the reconciler
// (Plan 09 Task 6) when FS reports a "channels in use" value that differs
// from our local counter. n is clamped to [0, +∞) — a negative input is
// treated as 0 (no point storing a negative counter that subsequent
// TryAcquire would just compare against cap as zero anyway).
func (b *Backpressure) SetActiveChannels(ctx context.Context, node string, n int) error {
	if n < 0 {
		n = 0
	}
	if err := b.rdb.Set(ctx, b.key(node), n, b.ttl).Err(); err != nil {
		return fmt.Errorf("router/backpressure: set active channels %s: %w", node, err)
	}
	return nil
}

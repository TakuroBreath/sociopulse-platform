package service

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Sliding-window rate limit per IP and per account, backed by Redis sorted
// sets. Spec §FR-A8 / §14.2 mandates 30 hits/IP/h and 10 hits/user/h with
// a rolling (NOT fixed-bucket) window — fixed buckets double-count at the
// boundary which makes them useless against an attacker who learns where
// the boundary is.
//
// Redis schema (also documented in plan-05-auth references):
//
//	auth:rl:ip:<ip>     sorted set  TTL=window+1m   score=unix-ms, member=<unix-nano>-<rand-hex>
//	auth:rl:user:<uuid> sorted set  TTL=window+1m   score=unix-ms, member=<unix-nano>-<rand-hex>
//
// TTL is window+1m to absorb clock skew between the application clock and
// the Redis server's wall clock — without the buffer, an entry a few
// milliseconds older than `window` would be pruned by ZRemRangeByScore but
// the key TTL would still be live, keeping the key alive for an extra
// millisecond and not affecting correctness, but wasting memory for the
// tail of the window.

// Default knobs for NewRateLimiterRedis when callers pass zero.
const (
	defaultPerIPPerHour   = 30
	defaultPerUserPerHour = 10
	defaultRateWindow     = time.Hour
	// rlMemberRandHexLen is the random suffix length appended to each
	// sorted-set member to guarantee uniqueness even when two requests
	// land on the same nanosecond. 8 hex chars (32 bits) makes a
	// collision astronomically improbable inside a 1-hour window.
	rlMemberRandHexLen = 4 // bytes -> 8 hex chars
)

// RateLimiterRedis is the Redis-backed implementation of RateLimiter.
//
// Concurrency: safe for concurrent use. Every operation runs as a single
// pipeline of {ZRemRangeByScore, ZCard, ZAdd, Expire}, where the count is
// read BEFORE the new entry is added — the comparison count<max is
// therefore correct (the Nth+1 attempt sees count==N and rejects, but the
// pipeline still adds it so a continued flood is correctly rate-limited).
type RateLimiterRedis struct {
	rdb            redis.UniversalClient
	perIPPerHour   int
	perUserPerHour int
	window         time.Duration
	clock          func() time.Time
}

// Compile-time guarantee the implementation satisfies the public contract.
var _ RateLimiter = (*RateLimiterRedis)(nil)

// NewRateLimiterRedis constructs a RateLimiterRedis. Zero values fall back
// to spec defaults: 30 hits/IP/h, 10 hits/user/h, 1-hour window. A nil
// clock falls back to time.Now.
//
// The rdb argument accepts redis.UniversalClient so the same constructor
// works for *redis.Client, *redis.ClusterClient, *redis.Ring, and the
// miniredis-backed client used in tests.
func NewRateLimiterRedis(
	rdb redis.UniversalClient,
	perIP, perUser int,
	window time.Duration,
	clock func() time.Time,
) *RateLimiterRedis {
	if perIP <= 0 {
		perIP = defaultPerIPPerHour
	}
	if perUser <= 0 {
		perUser = defaultPerUserPerHour
	}
	if window <= 0 {
		window = defaultRateWindow
	}
	if clock == nil {
		clock = time.Now
	}
	return &RateLimiterRedis{
		rdb:            rdb,
		perIPPerHour:   perIP,
		perUserPerHour: perUser,
		window:         window,
		clock:          clock,
	}
}

// AllowIP returns true when the supplied IP has fewer than perIPPerHour
// recorded attempts in the rolling window. Every call records an attempt
// (success or rejection), so a flood of requests is correctly clamped.
//
// A zero (invalid) netip.Addr does not panic — it stringifies as "invalid
// IP" and is treated as any other distinct key. Callers that care about
// IP validity should reject zero-Addr at the request boundary.
func (r *RateLimiterRedis) AllowIP(ctx context.Context, ip netip.Addr) (bool, error) {
	return r.allow(ctx, "auth:rl:ip:"+ip.String(), r.perIPPerHour)
}

// AllowAccount returns true when the supplied user id has fewer than
// perUserPerHour recorded attempts in the rolling window. Same semantics
// as AllowIP, keyed on the canonical UUID string.
func (r *RateLimiterRedis) AllowAccount(ctx context.Context, userID uuid.UUID) (bool, error) {
	return r.allow(ctx, "auth:rl:user:"+userID.String(), r.perUserPerHour)
}

// allow runs the pipeline:
//
//  1. ZRemRangeByScore prune entries older than (now - window).
//  2. ZCard count remaining (this is the count BEFORE adding the new entry).
//  3. ZAdd record the new attempt with score=unix-ms, unique member.
//  4. Expire bump TTL on the key (window+1m) so an idle key naturally GCs.
//
// The unique member is "<unix-nano>-<rand-hex>" — unix-nano alone is not
// safe under high concurrency on the same key (two goroutines on the same
// process can land on the same nanosecond), so we append a crypto-random
// suffix.
func (r *RateLimiterRedis) allow(ctx context.Context, key string, max int) (bool, error) {
	now := r.clock()
	windowStart := now.Add(-r.window).UnixMilli()

	suffix, err := randomHex(rlMemberRandHexLen)
	if err != nil {
		return false, fmt.Errorf("auth/service: rate-limit member: %w", err)
	}
	member := strconv.FormatInt(now.UnixNano(), 10) + "-" + suffix

	pipe := r.rdb.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "0", "("+strconv.FormatInt(windowStart, 10))
	countCmd := pipe.ZCard(ctx, key)
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now.UnixMilli()), Member: member})
	pipe.Expire(ctx, key, r.window+time.Minute)

	if _, err := pipe.Exec(ctx); err != nil {
		return false, fmt.Errorf("auth/service: rate-limit pipeline: %w", err)
	}

	return countCmd.Val() < int64(max), nil
}

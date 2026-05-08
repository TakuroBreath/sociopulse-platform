package rdd

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// leakBucketLua is the canonical token-bucket Lua script. Atomic across
// the read-decrement-write triplet so concurrent Generate calls for the
// same tenant cannot exceed the cap. Stored in a separate .lua so
// readability is preserved and the SHA1 cache is shared across
// Generator instances within one process.
//
// KEYS[1] = leak bucket key  (e.g. "rdd:leakbucket:<tenant_id>")
// ARGV[1] = capacity         (max tokens; refills at the same rate)
// ARGV[2] = refill rate      (tokens per second, integer)
// ARGV[3] = now              (caller-supplied unix-millis; tests use a
//                             frozen clock, prod uses the redis clock-
//                             agnostic millis from the queue layer)
// ARGV[4] = ttl              (seconds; refreshed on every successful
//                             update so an idle tenant's bucket key
//                             eventually ages out of redis)
//
// Returns:
//
//	1 — token consumed; caller proceeds.
//	0 — bucket empty; caller buckets the attempt as Throttled.

//go:embed lua/leak_bucket.lua
var leakBucketLua string

// leakBucketScript wraps the embedded source with redis.NewScript so the
// SHA1 cache survives across Generator calls. Plan 09 lesson #1 / Plan
// 10 references §"redis.NewScript" — never raw EVAL.
var leakBucketScript = redis.NewScript(leakBucketLua)

// LeakBucket is a per-tenant rate limiter backed by Redis. The
// implementation is the canonical "token bucket" — every successful
// Allow consumes one token; tokens refill at PerTenantPerSec per second
// up to the same cap. Plan 10 §"open Q" defaults the rate to 10/sec/
// tenant; the constructor parameter is kept on the surface so a tenant
// settings panel can override.
type LeakBucket struct {
	rdb       *redis.Client
	clock     func() time.Time
	rate      int           // tokens per second (== capacity for v1)
	ttl       time.Duration // bucket key TTL refreshed on every Allow
	keyPrefix string        // override-able for tests; "rdd:leakbucket" by default
}

// newLeakBucket constructs a LeakBucket. rate must be > 0; ttl falls
// back to 1h when zero (a per-tenant bucket that's idle for over 1h
// gets reset to full, which matches the "best-effort throttle" intent).
func newLeakBucket(rdb *redis.Client, rate int, ttl time.Duration, clock func() time.Time) *LeakBucket {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if clock == nil {
		clock = time.Now
	}
	return &LeakBucket{
		rdb:       rdb,
		clock:     clock,
		rate:      rate,
		ttl:       ttl,
		keyPrefix: "rdd:leakbucket",
	}
}

// key returns the canonical bucket key for the (tenant) pair. Cluster-
// safe: includes only one slot identifier so MULTI / Lua scripts always
// hash to the same node.
func (b *LeakBucket) key(tenantID uuid.UUID) string {
	return b.keyPrefix + ":" + tenantID.String()
}

// Allow attempts to consume one token from the tenant's bucket. Returns
// (true, nil) on success, (false, nil) when the bucket is empty (i.e.
// the tenant exceeded its rate), or (false, err) on Redis transport
// failure. Callers MUST treat Redis errors as conservative throttle —
// the alternative (silent burst-through) breaks the rate limit guard.
func (b *LeakBucket) Allow(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	nowMS := b.clock().UnixMilli()
	res, err := leakBucketScript.Run(
		ctx, b.rdb,
		[]string{b.key(tenantID)},
		strconv.Itoa(b.rate),               // capacity
		strconv.Itoa(b.rate),               // refill rate (per second)
		strconv.FormatInt(nowMS, 10),       // now in unix-millis
		strconv.Itoa(int(b.ttl.Seconds())), // ttl seconds
	).Int()
	if err != nil {
		return false, fmt.Errorf("rdd/leakbucket: run script: %w", err)
	}
	return res == 1, nil
}

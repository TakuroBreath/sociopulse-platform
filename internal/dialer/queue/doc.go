// Package queue implements api.CallQueue — the per-tenant per-project Redis
// ZSET that the dialer worker loop pops from to assign respondents to ready
// operators.
//
// # Storage layout
//
// Two Redis keys per (tenant, project):
//
//  1. q:<tenant_uuid>:project:<project_uuid> — sorted set whose members are
//     JSON-encoded QueueItem blobs and whose scores follow the formula
//     below. ZPOPMIN returns the lowest-scoring member, which by
//     construction is the most-urgent + oldest item.
//
//  2. qd:<tenant_uuid>:project:<project_uuid> — companion deduplication SET
//     holding bare respondent_uuid strings. EnqueueRespondent uses SADD
//     into this set as the "is this respondent already queued" check; PickNext
//     SREMs the popped item; Remove ZREMs from the ZSET and SREMs from this
//     set in one atomic Lua call.
//
// Both keys carry the same TTL so a queue silent for 24h vanishes from
// Redis cleanly. Every successful enqueue / requeue refreshes the TTL on
// both keys.
//
// # Score formula (lock this down)
//
// Score = priority * 1e9 + epoch_ms.
//
// Priority is 0..9 (lower = more urgent). The 1e9-millisecond gap between
// priorities exceeds 11 days of timestamp space, so a fresh priority-5 item
// never "beats" a stale priority-4 item. This is the crucial invariant
// that lets ZPOPMIN return "highest priority + oldest" in one O(log n) call.
//
// Implementation: float64(priority)*1e9 + float64(enqueuedAt.UnixMilli()).
// Redis ZSET stores 64-bit doubles; the formula stays exact within ±2^53
// integer precision, which is enough for the timestamp range until ~2105.
//
// Priority is clamped to [0, 9] on encode. Requeue caps at 9 specifically
// so a runaway retry loop does not cause arithmetic overflow on the float.
//
// # QueueItem JSON encoding
//
// ZSET membership is keyed off the raw JSON bytes — two enqueues with the
// same logical content but different JSON byte sequences would appear as
// two distinct ZSET members and the dedup SET would catch the second one,
// but the dangling member from the first would still pop. To eliminate that
// foot-gun the package marshals QueueItem with a deterministic, hand-rolled
// JSON helper (codec.go) that emits fields in a fixed order. encoding/json's
// struct-marshal preserves declaration order, but the project rule "if it
// matters, lock it down explicitly" applies — we do not depend on stdlib
// implementation behaviour for an on-the-wire invariant.
//
// # Lua atomicity
//
// All four operations (enqueue, pop_next, requeue, remove) execute as
// single Lua scripts via redis.NewScript. Redis serializes script execution
// per key, so the SADD-then-ZADD (and ZPOPMIN-then-SREM) sequences cannot
// race. The package never issues raw EVAL — go-redis caches the SHA1 the
// first time the script runs and uses EVALSHA on subsequent calls.
//
// All scripts touch a single (zset, dedup) key pair sharing the same
// {tenant, project} prefix; a future Cluster deployment must hash-tag the
// pair (e.g. q:{tenant:project} / qd:{tenant:project}) so both land in
// the same slot. v1 runs single-instance Redis so this is a forward note.
//
// # Concurrency
//
// PickNext is safe to call from N parallel workers. The ZPOPMIN-then-SREM
// Lua script is atomic, so the same item never pops twice. The integration
// test (redis_zset_integration_test.go) verifies the invariant against
// real Redis 7.4 with 10 goroutines × 100 pops.
//
// # Metrics
//
// Per-package Prometheus collectors live in metrics.go. RegisterMetrics
// builds and registers the collectors on a caller-supplied registerer; tests
// pass prometheus.NewRegistry(). Like Plan 09's package-private metrics, we
// deliberately do NOT register at init() time — two test imports of an init-
// registering package collide on prometheus.DefaultRegisterer.
package queue

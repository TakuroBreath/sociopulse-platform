package rdd

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// dedupMarkLua is the canonical SADD+EXPIRE script for the dedup tier.
// Atomic across the SADD-then-EXPIRE pair (one RTT instead of two via
// MULTI/EXEC) and shares the SHA1 cache across Generator instances
// within one process. Plan 09 lesson #1 / Plan 10 references — never
// raw EVAL.
//
// KEYS[1] = "rdd:seen:<tenant_id>"
// ARGV[1..N-1] = phone members (one or more E.164 strings)
// ARGV[N]      = ttl seconds
//
// Returns the SADD count (new members added).
//
//go:embed lua/dedup_mark.lua
var dedupMarkLua string

var dedupMarkScript = redis.NewScript(dedupMarkLua)

// Dedup is a two-tier deduplicator for RDD-generated phones. The Bloom
// filter is the cheap pre-filter (tenant-scope, in-process, lazily
// created on first touch); the Redis SET is the durable record with a
// TTL that survives process restarts (tenant-scope, default 30 days).
//
// Both tiers share the SAME tenant-scope so the pre-filter semantics
// hold: a Bloom miss is definitive ("never seen for this tenant") and
// no Redis round-trip is required. The plan body originally proposed a
// project-scope Bloom paired with a tenant-scope SET; that mixed
// scoping breaks the pre-filter property because a cross-project tenant
// duplicate would slip through the project Bloom and skip Redis. Using
// the same scope for both tiers keeps correctness and fast-path latency
// aligned. Project IDs flow through the API for consistency with the
// rest of the dialer module — but the dedup logic itself is tenant-
// scoped, matching the Redis SET key shape.
//
// Bloom filters never report a false negative, so a "miss" in Bloom
// definitively means the phone was never seen — we skip the Redis
// round-trip entirely. A "hit" in Bloom is either a true positive (we
// generated this phone before) or a false positive (rare; tunable via
// [Limits.BloomFPRate]); we confirm the hit against the Redis SET so
// a unique phone is never thrown away on a Bloom collision.
type Dedup struct {
	rdb       *redis.Client
	cap       uint    // bloom filter capacity per tenant
	fpr       float64 // bloom filter target false-positive rate
	ttl       time.Duration
	keyPrefix string // override-able; "rdd:seen" by default

	mu      sync.RWMutex
	filters map[uuid.UUID]*lockedFilter
}

// lockedFilter wraps a [bloom.BloomFilter] with a mutex. The bloom
// library is NOT concurrency-safe for Add/Test on the same instance,
// so concurrent Generate calls touching the same tenant must serialise
// their access. The lock is held across single-element operations only;
// it never spans I/O.
type lockedFilter struct {
	mu sync.Mutex
	f  *bloom.BloomFilter
}

func (l *lockedFilter) Add(b []byte) {
	l.mu.Lock()
	l.f.Add(b)
	l.mu.Unlock()
}

func (l *lockedFilter) Test(b []byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Test(b)
}

// newDedup constructs a Dedup. capacity / fpRate apply to every
// per-tenant Bloom filter that the dedup spawns lazily on first
// touch. ttl is the Redis SET TTL — refreshed on every successful Add.
func newDedup(rdb *redis.Client, capacity uint, fpRate float64, ttl time.Duration) *Dedup {
	return &Dedup{
		rdb:       rdb,
		cap:       capacity,
		fpr:       fpRate,
		ttl:       ttl,
		keyPrefix: "rdd:seen",
		filters:   make(map[uuid.UUID]*lockedFilter),
	}
}

// setKey returns the canonical Redis key for the tenant's seen-phones
// SET. Tenant-scoped (not project-scoped) so a project rotated /
// archived / re-imported does not lose the dedup history — the regulator
// reading the audit trail expects "we never re-dialled this phone in
// the last 30 days regardless of which project owned it".
func (d *Dedup) setKey(tenantID uuid.UUID) string {
	return d.keyPrefix + ":" + tenantID.String()
}

// filterFor returns the Bloom filter for the tenant, lazily allocated
// on first touch and pre-seeded from the durable Redis SET so a fresh
// Generator process honours dedup history written by a peer instance
// or a previous run. Concurrency-safe — the upgrade path holds the
// write lock for the duration of the SSCAN bootstrap so concurrent
// callers see a fully-populated Bloom on the first observed value.
//
// A SSCAN failure during the bootstrap surfaces as a fmt.Errorf
// wrapping ctx + Redis transport error so the caller can decide
// whether to abort the run or continue with a degraded filter; the
// implementation falls back to an empty Bloom when ctx is cancelled
// (the Seen path then reverts to "always confirm via Redis").
func (d *Dedup) filterFor(ctx context.Context, tenantID uuid.UUID) (*lockedFilter, error) {
	d.mu.RLock()
	if lf, ok := d.filters[tenantID]; ok {
		d.mu.RUnlock()
		return lf, nil
	}
	d.mu.RUnlock()

	d.mu.Lock()
	defer d.mu.Unlock()
	if lf, ok := d.filters[tenantID]; ok {
		return lf, nil
	}
	lf := &lockedFilter{f: bloom.NewWithEstimates(d.cap, d.fpr)}
	// Bootstrap from Redis: SSCAN every member of the durable SET into
	// the freshly-allocated Bloom. The cursor loop exits cleanly on an
	// empty SET (cursor==0 on first call) and on a cancelled context.
	// We hold d.mu for the duration so concurrent first-touch callers
	// observe the populated filter under the upgrade-lock contract.
	var cursor uint64
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("rdd/dedup: bootstrap ctx: %w", err)
		}
		members, next, err := d.rdb.SScan(ctx, d.setKey(tenantID), cursor, "", scanCount).Result()
		if err != nil {
			// Network failure during bootstrap — store a partially-
			// loaded filter so we don't busy-loop on every call, but
			// surface the error to the caller so the operator sees the
			// degraded state.
			d.filters[tenantID] = lf
			return nil, fmt.Errorf("rdd/dedup: sscan bootstrap: %w", err)
		}
		// During bootstrap we hold the upgrade lock and no other goroutine
		// can see lf yet, so direct Add() on the inner filter is safe and
		// avoids per-element lock acquisition.
		for _, m := range members {
			lf.f.Add([]byte(m))
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	d.filters[tenantID] = lf
	return lf, nil
}

// scanCount is the SSCAN COUNT hint. 1k is the canonical "balance
// between fewer round-trips and per-call latency" figure for SSCAN.
// Tuning: a large COUNT increases per-call latency on huge sets; a
// small COUNT increases the number of round-trips. v1 expects
// per-tenant SETs in the 0–10k range so 1k is one or two round-trips.
const scanCount = 1000

// Seen returns true when the phone has already been generated for this
// tenant. The path is:
//
//  1. Test the tenant's Bloom filter (lazily bootstrapped from Redis on
//     first touch) — a clean miss is definitive (Bloom has zero false
//     negatives over the loaded data) and we skip a Redis round-trip.
//  2. On a Bloom hit, confirm against the Redis SET — Bloom may false-
//     positive at the configured rate; the SET is authoritative.
//
// projectID is accepted on the surface for API consistency but the
// dedup key is tenant-scoped — see the type-doc rationale on Dedup.
//
// Redis transport errors propagate to the caller; the generator buckets
// such failures as InvalidHit (we can't confirm uniqueness, so we
// conservatively skip).
func (d *Dedup) Seen(ctx context.Context, tenantID, _ uuid.UUID, phone string) (bool, error) {
	lf, err := d.filterFor(ctx, tenantID)
	if err != nil {
		return false, err
	}
	if !lf.Test([]byte(phone)) {
		return false, nil
	}
	hit, err := d.rdb.SIsMember(ctx, d.setKey(tenantID), phone).Result()
	if err != nil {
		return false, fmt.Errorf("rdd/dedup: sismember: %w", err)
	}
	return hit, nil
}

// Mark records the phone as seen in BOTH tiers. The Bloom write is
// strictly local (no I/O once bootstrapped); the SET write hits Redis
// via the dedup_mark Lua script — atomic SADD + EXPIRE in a single
// round-trip. The order matters: we Add to Bloom first (cheap, can't
// fail) so even if the Redis write trips a transient transport error
// the in-process generator still treats the phone as taken for the
// remainder of the run.
//
// Returns the number of NEW members added to the Redis SET — 1 for a
// fresh phone, 0 for a re-mark of an already-tracked phone. Callers
// generally ignore the count; tests use it to assert idempotency.
//
// projectID is accepted for API consistency; the dedup keys are
// tenant-scoped per the type-doc rationale.
func (d *Dedup) Mark(ctx context.Context, tenantID, _ uuid.UUID, phone string) error {
	_, err := d.MarkN(ctx, tenantID, phone)
	return err
}

// MarkN is the count-returning variant of Mark used by tests that need
// to assert SADD propagation. Returns the number of new members
// inserted into the durable Redis SET (0 means the phone was already
// in the SET — idempotent re-mark).
func (d *Dedup) MarkN(ctx context.Context, tenantID uuid.UUID, phone string) (int64, error) {
	// filterFor's contract on error:
	//   - ctx canceled / deadline exceeded → returns (nil, ctxErr).
	//     Nothing to do for the Bloom tier; the Redis script call below
	//     will surface the same ctx error.
	//   - SSCAN bootstrap failure → stores a partial filter under
	//     d.filters[tenantID] for the next caller to refresh, but
	//     returns (nil, err) here so we don't risk double-adding to a
	//     filter whose contents we can't trust.
	//
	// In both cases lf is nil; the nil-check below skips the Bloom Add
	// and we let the Redis script run regardless. The script is the
	// authoritative tier — Bloom is the fast-path cache.
	lf, _ := d.filterFor(ctx, tenantID)
	if lf != nil {
		lf.Add([]byte(phone))
	}
	added, err := dedupMarkScript.Run(
		ctx, d.rdb,
		[]string{d.setKey(tenantID)},
		phone,
		strconv.Itoa(int(d.ttl.Seconds())),
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("rdd/dedup: mark script: %w", err)
	}
	return added, nil
}

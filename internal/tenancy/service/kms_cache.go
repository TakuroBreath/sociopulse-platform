package service

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DEKCacheKey addresses one cached DEK entry. KEK rotation produces a new
// KEKVersion, which yields a fresh cache slot — the old entry remains
// resident for read-paths that are still circulating wrapped DEKs from the
// previous version, and ages out via TTL.
type DEKCacheKey struct {
	TenantID   uuid.UUID
	KEKVersion string
}

// CachedDEK is the value side of the cache: an unwrapped DEK plaintext, the
// matching wrapped form so the Decrypt fast-path can compare against it
// without re-calling KMS, and the KEK version that wrapped this DEK.
//
// CRITICAL: callers must not mutate Plaintext after handing the entry to
// the cache. The cache zeroises Plaintext on eviction (best-effort — Go's
// GC may move the backing array; the zeroisation only clears the visible
// reference, not orphaned copies the runtime made earlier).
type CachedDEK struct {
	Plaintext  []byte // 32 bytes (AES-256)
	Ciphertext []byte // KMS-wrapped DEK, stored alongside the payload
	KeyVersion string // copy of DEKCacheKey.KEKVersion for convenience
}

// DEKCacheConfig configures the cache. The zero value is valid: it yields
// a 1024-entry LRU with 5-minute TTL and a 30-second eviction tick. Override
// Now (a clock indirection) in tests so TTL behaviour is deterministic.
type DEKCacheConfig struct {
	// Size caps the number of resident entries. When full, Put evicts the
	// least-recently-used entry before inserting.
	Size int

	// TTL is how long a fresh entry remains resident. Both lazy expiry
	// (on Get) and proactive sweep (eviction goroutine) honour this bound.
	TTL time.Duration

	// TickRate is how often the eviction goroutine wakes to sweep.
	// Default: TTL/4, clamped to [1s, 1m]. Tests pass small values.
	TickRate time.Duration

	// Now is the clock indirection. Production leaves this nil (we fall
	// through to time.Now); tests inject a fake clock.
	Now func() time.Time
}

func (c *DEKCacheConfig) defaults() {
	if c.Size <= 0 {
		c.Size = 1024
	}
	if c.TTL <= 0 {
		c.TTL = 5 * time.Minute
	}
	if c.TickRate <= 0 {
		c.TickRate = c.TTL / 4
		if c.TickRate < time.Second {
			c.TickRate = time.Second
		}
		if c.TickRate > time.Minute {
			c.TickRate = time.Minute
		}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// dekCacheItem is the payload threaded through the LRU's doubly-linked list.
// We keep the key inside the item so we can reverse-lookup from a list
// element back to its map entry during eviction without a second pass.
type dekCacheItem struct {
	key       DEKCacheKey
	dek       *CachedDEK
	expiresAt time.Time
}

// itemFrom returns the *dekCacheItem held by a list.Element. The cache
// owns every element it inserts, so the type assertion cannot fail; the
// helper exists only to centralise the assertion and silence the
// forcetypeassert lint without sprinkling _, _ = ... assertions through
// the hot path.
func itemFrom(el *list.Element) *dekCacheItem {
	if el == nil {
		return nil
	}
	it, _ := el.Value.(*dekCacheItem)
	return it
}

// DEKCache is a thread-safe LRU cache with TTL expiry. Reads (Get) take
// the write-lock because they update the recency list — RWMutex would only
// help if Get were truly read-only. Throughput is acceptable: encrypt/decrypt
// are dominated by AES-GCM, and the cache is sized to a small constant.
//
// Lifecycle:
//   - NewDEKCache spawns one eviction goroutine; Stop tears it down.
//   - NewDEKCacheWithContext binds the goroutine's exit to ctx.Done.
//
// Both lifetimes terminate the goroutine cleanly so goleak stays green.
type DEKCache struct {
	cfg DEKCacheConfig

	mu    sync.Mutex
	items map[DEKCacheKey]*list.Element // map → linked-list node for O(1) lookup
	lru   *list.List                    // front = MRU, back = LRU

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewDEKCache constructs a cache with its own internal cancel channel. The
// caller must call Stop to terminate the eviction goroutine.
func NewDEKCache(cfg DEKCacheConfig) *DEKCache {
	return NewDEKCacheWithContext(context.Background(), cfg)
}

// NewDEKCacheWithContext binds the eviction goroutine's lifetime to ctx.
// Cancelling ctx is equivalent to calling Stop.
func NewDEKCacheWithContext(ctx context.Context, cfg DEKCacheConfig) *DEKCache {
	cfg.defaults()
	c := &DEKCache{
		cfg:   cfg,
		items: make(map[DEKCacheKey]*list.Element, cfg.Size),
		lru:   list.New(),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go c.runEviction(ctx)
	return c
}

// Stop terminates the eviction goroutine. Idempotent: calling twice is
// safe. After Stop returns, the cache may still be used (Get/Put still
// work), but expired entries will only be cleared lazily on Get.
func (c *DEKCache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stop)
	})
	// Wait for the eviction goroutine to exit so goleak stays clean.
	<-c.done
}

// Get returns the entry under key, or (nil, false) if missing/expired.
// A hit promotes the entry to most-recently-used; a stale entry is removed
// (lazy expiry) so subsequent Gets return clean misses.
func (c *DEKCache) Get(key DEKCacheKey) (*CachedDEK, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	it := itemFrom(el)
	if c.expired(it) {
		c.removeElementLocked(el)
		return nil, false
	}
	c.lru.MoveToFront(el)
	return it.dek, true
}

// Put inserts (or replaces) the entry. If the cache is at capacity the
// least-recently-used entry is evicted first. The entry's TTL deadline is
// computed from the current clock.
func (c *DEKCache) Put(key DEKCacheKey, dek *CachedDEK) {
	if dek == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		// Replacing — zero the old plaintext so it doesn't linger.
		old := itemFrom(el)
		zeroPlaintext(old.dek)
		old.dek = dek
		old.expiresAt = c.cfg.Now().Add(c.cfg.TTL)
		c.lru.MoveToFront(el)
		return
	}
	for c.lru.Len() >= c.cfg.Size {
		c.evictOldestLocked()
	}
	it := &dekCacheItem{
		key:       key,
		dek:       dek,
		expiresAt: c.cfg.Now().Add(c.cfg.TTL),
	}
	el := c.lru.PushFront(it)
	c.items[key] = el
}

// InvalidateTenant drops every entry whose tenant ID matches. Used by
// Suspend/Archive (operator forces re-fetch on next access) and by KEK
// rotation (defence in depth — TTL would clear them anyway).
func (c *DEKCache) InvalidateTenant(tenantID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, el := range c.items {
		if k.TenantID == tenantID {
			c.removeElementLocked(el)
		}
	}
}

// Len returns the number of resident entries — primarily used by tests
// that observe the eviction goroutine's effect.
func (c *DEKCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// GetByTenant returns the most-recently-used resident DEK for the tenant
// across all KEK versions, or (nil, false) if no fresh entry exists. A hit
// promotes the entry to MRU. Stale entries discovered along the way are
// removed (lazy expiry). Worst case: O(n) over the LRU tail when only the
// oldest matches; typical case: the MRU front is the answer.
//
// The resolver uses this for the encrypt-path lookup — it doesn't yet
// know which KEK version the caller wants; the cache returns whatever is
// freshest. KEK rotation invalidates explicitly via InvalidateTenant.
func (c *DEKCache) GetByTenant(tenantID uuid.UUID) (*CachedDEK, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		it := itemFrom(el)
		if it.key.TenantID != tenantID {
			el = next
			continue
		}
		if c.expired(it) {
			c.removeElementLocked(el)
			el = next
			continue
		}
		c.lru.MoveToFront(el)
		return it.dek, true
	}
	return nil, false
}

// expired reports whether it is past its TTL deadline. Caller must hold mu.
func (c *DEKCache) expired(it *dekCacheItem) bool {
	return c.cfg.Now().After(it.expiresAt)
}

// evictOldestLocked drops the LRU tail. Caller must hold mu.
func (c *DEKCache) evictOldestLocked() {
	tail := c.lru.Back()
	if tail == nil {
		return
	}
	c.removeElementLocked(tail)
}

// removeElementLocked deletes one entry, zeroing its plaintext along the
// way. Caller must hold mu.
func (c *DEKCache) removeElementLocked(el *list.Element) {
	it := itemFrom(el)
	zeroPlaintext(it.dek)
	delete(c.items, it.key)
	c.lru.Remove(el)
}

// runEviction sweeps expired entries on a fixed cadence. Exits when either
// Stop is called (c.stop closed) or ctx is cancelled. Uses time.NewTicker —
// not time.After in a loop — to avoid leaking a timer per iteration
// (golang-concurrency BP8).
func (c *DEKCache) runEviction(ctx context.Context) {
	defer close(c.done)
	ticker := time.NewTicker(c.cfg.TickRate)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweepExpired()
		}
	}
}

// sweepExpired walks the cache once and drops every entry whose deadline
// has passed. Worst case: O(n); the cache is sized to ~1k entries so this
// is well below the eviction-tick budget.
func (c *DEKCache) sweepExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, el := range c.items {
		it := itemFrom(el)
		if c.expired(it) {
			zeroPlaintext(it.dek)
			delete(c.items, k)
			c.lru.Remove(el)
		}
	}
}

// zeroPlaintext writes zeros over the DEK plaintext slice. Best-effort:
// the Go runtime may have moved the underlying bytes earlier; we can only
// clear the slice we still hold. Documented for callers in package docs.
func zeroPlaintext(dek *CachedDEK) {
	if dek == nil {
		return
	}
	for i := range dek.Plaintext {
		dek.Plaintext[i] = 0
	}
}

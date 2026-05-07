package service_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/sociopulse/platform/internal/tenancy/service"
)

// fakeClock is a deterministic clock used by the cache tests so that TTL
// behaviour can be verified without real-time sleeps. The eviction goroutine
// is exercised by separate tests that use a real clock with a short tick.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// makeKey is a tiny helper so each test reads cleanly.
func makeKey(id uuid.UUID, version string) service.DEKCacheKey {
	return service.DEKCacheKey{TenantID: id, KEKVersion: version}
}

func makeEntry(b byte) *service.CachedDEK {
	pt := make([]byte, 32)
	for i := range pt {
		pt[i] = b
	}
	return &service.CachedDEK{
		Plaintext:  pt,
		Ciphertext: []byte{b, b, b},
		KeyVersion: "v1",
	}
}

func TestDEKCache_GetReturnsMissOnEmptyCache(t *testing.T) {
	t.Parallel()

	c := service.NewDEKCache(service.DEKCacheConfig{Size: 4, TTL: time.Hour})
	defer c.Stop()

	got, ok := c.Get(makeKey(uuid.New(), "v1"))
	require.False(t, ok, "empty cache must miss")
	require.Nil(t, got)
}

func TestDEKCache_PutThenGetReturnsHit(t *testing.T) {
	t.Parallel()

	c := service.NewDEKCache(service.DEKCacheConfig{Size: 4, TTL: time.Hour})
	defer c.Stop()

	k := makeKey(uuid.New(), "v1")
	want := makeEntry(7)
	c.Put(k, want)

	got, ok := c.Get(k)
	require.True(t, ok, "Put then Get must hit")
	require.Equal(t, want.Plaintext, got.Plaintext)
	require.Equal(t, want.Ciphertext, got.Ciphertext)
	require.Equal(t, want.KeyVersion, got.KeyVersion)
}

func TestDEKCache_LRU_EvictsOldestWhenFull(t *testing.T) {
	t.Parallel()

	const cap = 3
	c := service.NewDEKCache(service.DEKCacheConfig{Size: cap, TTL: time.Hour})
	defer c.Stop()

	keys := make([]service.DEKCacheKey, 0, cap+1)
	for i := 0; i < cap+1; i++ {
		k := makeKey(uuid.New(), "v1")
		keys = append(keys, k)
		c.Put(k, makeEntry(byte(i)))
	}

	// First key inserted is the oldest — must be evicted.
	_, ok := c.Get(keys[0])
	require.False(t, ok, "the oldest entry must be evicted when capacity is exceeded")

	// All later keys must still be present.
	for i := 1; i < len(keys); i++ {
		_, ok := c.Get(keys[i])
		require.Truef(t, ok, "entry %d should remain after one eviction", i)
	}
}

func TestDEKCache_LRU_GetPromotesToMostRecentlyUsed(t *testing.T) {
	t.Parallel()

	const cap = 3
	c := service.NewDEKCache(service.DEKCacheConfig{Size: cap, TTL: time.Hour})
	defer c.Stop()

	a := makeKey(uuid.New(), "v1")
	b := makeKey(uuid.New(), "v1")
	d := makeKey(uuid.New(), "v1")
	c.Put(a, makeEntry(1))
	c.Put(b, makeEntry(2))
	c.Put(d, makeEntry(3))

	// Touch `a` so it becomes most-recently-used.
	_, _ = c.Get(a)

	// Insert one more entry — the LRU must evict `b`, not `a`.
	e := makeKey(uuid.New(), "v1")
	c.Put(e, makeEntry(4))

	_, okA := c.Get(a)
	_, okB := c.Get(b)
	require.True(t, okA, "promoted entry `a` must survive")
	require.False(t, okB, "non-promoted entry `b` must be evicted")
}

func TestDEKCache_TTL_ExpiresOnGet(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	c := service.NewDEKCache(service.DEKCacheConfig{
		Size: 4,
		TTL:  10 * time.Millisecond,
		Now:  clk.Now,
	})
	defer c.Stop()

	k := makeKey(uuid.New(), "v1")
	c.Put(k, makeEntry(7))

	clk.advance(20 * time.Millisecond)

	_, ok := c.Get(k)
	require.False(t, ok, "expired entry must miss on Get (lazy expiry)")
}

func TestDEKCache_TTL_FreshEntryHits(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	c := service.NewDEKCache(service.DEKCacheConfig{
		Size: 4,
		TTL:  10 * time.Millisecond,
		Now:  clk.Now,
	})
	defer c.Stop()

	k := makeKey(uuid.New(), "v1")
	c.Put(k, makeEntry(7))

	clk.advance(5 * time.Millisecond) // not yet expired

	_, ok := c.Get(k)
	require.True(t, ok, "fresh entry must hit before TTL elapses")
}

func TestDEKCache_KEKVersion_KeysAreSeparate(t *testing.T) {
	t.Parallel()

	c := service.NewDEKCache(service.DEKCacheConfig{Size: 4, TTL: time.Hour})
	defer c.Stop()

	id := uuid.New()
	k1 := makeKey(id, "v1")
	k2 := makeKey(id, "v2")

	c.Put(k1, makeEntry(1))
	c.Put(k2, makeEntry(2))

	got1, ok1 := c.Get(k1)
	require.True(t, ok1)
	got2, ok2 := c.Get(k2)
	require.True(t, ok2)

	require.NotEqual(t, got1.Plaintext, got2.Plaintext,
		"different KEK versions must address different cache entries")
}

func TestDEKCache_Invalidate_RemovesAllEntriesForTenant(t *testing.T) {
	t.Parallel()

	c := service.NewDEKCache(service.DEKCacheConfig{Size: 8, TTL: time.Hour})
	defer c.Stop()

	idA := uuid.New()
	idB := uuid.New()
	c.Put(makeKey(idA, "v1"), makeEntry(1))
	c.Put(makeKey(idA, "v2"), makeEntry(2))
	c.Put(makeKey(idB, "v1"), makeEntry(3))

	c.InvalidateTenant(idA)

	_, ok := c.Get(makeKey(idA, "v1"))
	require.False(t, ok, "v1 entry for invalidated tenant must be gone")
	_, ok = c.Get(makeKey(idA, "v2"))
	require.False(t, ok, "v2 entry for invalidated tenant must be gone")
	_, ok = c.Get(makeKey(idB, "v1"))
	require.True(t, ok, "other tenant's entry must survive InvalidateTenant")
}

func TestDEKCache_Invalidate_ZeroesPlaintext(t *testing.T) {
	t.Parallel()

	c := service.NewDEKCache(service.DEKCacheConfig{Size: 4, TTL: time.Hour})
	defer c.Stop()

	k := makeKey(uuid.New(), "v1")
	pt := make([]byte, 32)
	for i := range pt {
		pt[i] = 0xAB
	}
	c.Put(k, &service.CachedDEK{Plaintext: pt, Ciphertext: []byte{1}, KeyVersion: "v1"})

	c.InvalidateTenant(k.TenantID)

	// pt is the slice we handed to the cache; after eviction it must be zero.
	allZero := true
	for _, b := range pt {
		if b != 0 {
			allZero = false
			break
		}
	}
	require.True(t, allZero, "DEK plaintext must be zeroed on invalidation (best-effort)")
}

func TestDEKCache_ConcurrentAccess_NoRace(t *testing.T) {
	t.Parallel()

	const goroutines = 8
	const opsPer = 200

	c := service.NewDEKCache(service.DEKCacheConfig{Size: 64, TTL: time.Hour})
	defer c.Stop()

	tenants := make([]uuid.UUID, 16)
	for i := range tenants {
		tenants[i] = uuid.New()
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	var totalOps atomic.Int64
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPer; i++ {
				k := makeKey(tenants[(g+i)%len(tenants)], "v1")
				if i%3 == 0 {
					c.Put(k, makeEntry(byte(g+i)))
				} else {
					_, _ = c.Get(k)
				}
				totalOps.Add(1)
			}
		}(g)
	}
	wg.Wait()
	require.Equal(t, int64(goroutines*opsPer), totalOps.Load())
}

//nolint:paralleltest // intentionally serial: real-clock timing dependency
func TestDEKCache_EvictionGoroutine_RemovesExpiredEntries(t *testing.T) {
	// This test uses a real clock with a tight tick + TTL so the periodic
	// eviction goroutine has a chance to run. It is deliberately NOT
	// t.Parallel because of the timing dependency — keeping it serial avoids
	// flakiness under parallel test pressure.

	c := service.NewDEKCache(service.DEKCacheConfig{
		Size:     4,
		TTL:      20 * time.Millisecond,
		TickRate: 5 * time.Millisecond,
	})
	defer c.Stop()

	k := makeKey(uuid.New(), "v1")
	c.Put(k, makeEntry(7))
	require.Equal(t, 1, c.Len())

	// Wait for the eviction tick to clear the expired entry.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.Len() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, 0, c.Len(),
		"periodic eviction goroutine must drop expired entries")
}

//nolint:paralleltest // serial: this test runs goleak.VerifyNone which scans live goroutines
func TestDEKCache_Stop_TerminatesEvictionGoroutineNoLeak(t *testing.T) {
	// We run goleak against this single test to assert the eviction goroutine
	// exits when Stop is called. The package-level goleak.VerifyTestMain
	// also catches leaks, but this asserts the contract directly.
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
	)

	c := service.NewDEKCache(service.DEKCacheConfig{
		Size:     4,
		TTL:      50 * time.Millisecond,
		TickRate: 10 * time.Millisecond,
	})
	c.Put(makeKey(uuid.New(), "v1"), makeEntry(1))
	c.Stop()
	// Sleep a beat to give the goroutine room to exit before goleak runs.
	time.Sleep(20 * time.Millisecond)
}

func TestDEKCache_Stop_IsIdempotent(t *testing.T) {
	t.Parallel()

	c := service.NewDEKCache(service.DEKCacheConfig{Size: 4, TTL: time.Hour})
	c.Stop()
	c.Stop() // second call must not panic
}

//nolint:paralleltest // serial: this test runs goleak.VerifyNone which scans live goroutines
func TestDEKCache_ContextCancel_StopsEvictionGoroutine(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	ctx, cancel := context.WithCancel(context.Background())
	c := service.NewDEKCacheWithContext(ctx, service.DEKCacheConfig{
		Size:     4,
		TTL:      50 * time.Millisecond,
		TickRate: 10 * time.Millisecond,
	})

	c.Put(makeKey(uuid.New(), "v1"), makeEntry(1))
	cancel()
	// Give the goroutine a moment to observe ctx.Done and exit.
	time.Sleep(30 * time.Millisecond)
}

// Compile-time guard so the test file forces these public symbols to exist.
var (
	_ = (*service.DEKCache)(nil)
	_ = service.DEKCacheKey{}
	_ = service.CachedDEK{}
	_ = service.DEKCacheConfig{}
)

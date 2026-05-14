package service_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/analytics/service"
)

// TestDedupLRU_AddReportsDuplicates asserts the Add contract:
//   - newly-inserted ids return false
//   - the SAME id supplied a second time returns true (dup hit)
//   - re-adding a dup keeps the id in the LRU (does not evict it)
func TestDedupLRU_AddReportsDuplicates(t *testing.T) {
	t.Parallel()

	lru := service.NewDedupLRU(8)
	id := uuid.New()

	require.False(t, lru.Add(id), "first Add should report newly-inserted")
	require.True(t, lru.Add(id), "second Add of same id should report duplicate")
	require.True(t, lru.Has(id), "id must still be present after dup hit")
	require.Equal(t, 1, lru.Len(), "single id => Len=1 regardless of dup adds")
}

// TestDedupLRU_EvictsOldestAtCapacity asserts LRU eviction: with cap=3,
// inserting four distinct ids in order [a,b,c,d] evicts `a`.
// Touching `b` via Add (dup hit) before inserting `d` should evict `c`
// instead — the touched element moves to MRU.
func TestDedupLRU_EvictsOldestAtCapacity(t *testing.T) {
	t.Parallel()

	t.Run("plain_lru_evicts_oldest", func(t *testing.T) {
		t.Parallel()
		lru := service.NewDedupLRU(3)
		a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		require.False(t, lru.Add(a))
		require.False(t, lru.Add(b))
		require.False(t, lru.Add(c))
		require.Equal(t, 3, lru.Len())
		require.False(t, lru.Add(d)) // forces eviction of a
		require.Equal(t, 3, lru.Len())

		require.False(t, lru.Has(a), "a should be evicted (oldest)")
		require.True(t, lru.Has(b))
		require.True(t, lru.Has(c))
		require.True(t, lru.Has(d))
	})

	t.Run("touched_id_survives_eviction", func(t *testing.T) {
		t.Parallel()
		lru := service.NewDedupLRU(3)
		a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		require.False(t, lru.Add(a))
		require.False(t, lru.Add(b))
		require.False(t, lru.Add(c))
		// Dup-hit on `a` should promote it to MRU; subsequent insert of d
		// evicts `b` (now oldest) instead of `a`.
		require.True(t, lru.Add(a))
		require.False(t, lru.Add(d))
		require.Equal(t, 3, lru.Len())

		require.True(t, lru.Has(a), "a was promoted, must survive")
		require.False(t, lru.Has(b), "b is now the oldest, evicted")
		require.True(t, lru.Has(c))
		require.True(t, lru.Has(d))
	})
}

// TestDedupLRU_HasDoesNotPromote asserts Has is a strict read — it does
// NOT change LRU ordering. With cap=2 and inserts [a,b], calling Has(a)
// then Add(c) must evict `a`, not `b` — Has must not have promoted `a`.
func TestDedupLRU_HasDoesNotPromote(t *testing.T) {
	t.Parallel()

	lru := service.NewDedupLRU(2)
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	require.False(t, lru.Add(a))
	require.False(t, lru.Add(b))
	require.True(t, lru.Has(a), "Has(a) returns true")
	// If Has promoted a, then inserting c would evict b.
	// If Has did NOT promote a, then inserting c evicts a (the older).
	require.False(t, lru.Add(c))

	require.False(t, lru.Has(a), "a should be evicted (Has must not promote)")
	require.True(t, lru.Has(b))
	require.True(t, lru.Has(c))
}

// TestDedupLRU_ConcurrentSafe stresses the mutex: 8 goroutines × 1000
// ops each, mixing Add + Has. The test asserts no panic, no data race
// (-race), and that final Len() never exceeds capacity.
func TestDedupLRU_ConcurrentSafe(t *testing.T) {
	t.Parallel()

	const (
		goroutines  = 8
		opsPerGo    = 1000
		capacityCap = 256
	)
	lru := service.NewDedupLRU(capacityCap)

	// Pre-build a pool of ids so workers collide on the same keys —
	// that's what exercises the dup-detection path under contention.
	ids := make([]uuid.UUID, 64)
	for i := range ids {
		ids[i] = uuid.New()
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(seed int) {
			defer wg.Done()
			for i := range opsPerGo {
				id := ids[(seed+i)%len(ids)]
				if i%2 == 0 {
					_ = lru.Add(id)
				} else {
					_ = lru.Has(id)
				}
			}
		}(g)
	}
	wg.Wait()

	require.LessOrEqual(t, lru.Len(), capacityCap, "Len must never exceed capacity")
	require.Positive(t, lru.Len(), "Len should be positive after ops")
}

// TestNewDedupLRU_PanicsOnNonPositiveCapacity asserts the constructor
// rejects capacity ≤ 0. A zero-capacity LRU would either trivially
// evict every insertion (useless) or wedge in the eviction branch
// (off-by-one bug surface) — panicking is the loudest signal.
func TestNewDedupLRU_PanicsOnNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() { _ = service.NewDedupLRU(0) }, "capacity=0 must panic")
	require.Panics(t, func() { _ = service.NewDedupLRU(-1) }, "capacity<0 must panic")
}

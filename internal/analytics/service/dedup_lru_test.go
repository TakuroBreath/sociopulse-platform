package service_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/analytics/service"
)

// TestDedupLRU_AddReportsDuplicates asserts the Add contract:
//   - newly-inserted ids return Dup=false
//   - the SAME id supplied a second time returns Dup=true (dup hit)
//   - re-adding a dup keeps the id in the LRU (does not evict it)
func TestDedupLRU_AddReportsDuplicates(t *testing.T) {
	t.Parallel()

	lru := service.NewDedupLRU(8)
	id := uuid.New()

	require.False(t, lru.Add(id).Dup, "first Add should report newly-inserted")
	require.True(t, lru.Add(id).Dup, "second Add of same id should report duplicate")
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
		require.False(t, lru.Add(a).Dup)
		require.False(t, lru.Add(b).Dup)
		require.False(t, lru.Add(c).Dup)
		require.Equal(t, 3, lru.Len())
		require.False(t, lru.Add(d).Dup) // forces eviction of a
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
		require.False(t, lru.Add(a).Dup)
		require.False(t, lru.Add(b).Dup)
		require.False(t, lru.Add(c).Dup)
		// Dup-hit on `a` should promote it to MRU; subsequent insert of d
		// evicts `b` (now oldest) instead of `a`.
		require.True(t, lru.Add(a).Dup)
		require.False(t, lru.Add(d).Dup)
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
	require.False(t, lru.Add(a).Dup)
	require.False(t, lru.Add(b).Dup)
	require.True(t, lru.Has(a), "Has(a) returns true")
	// If Has promoted a, then inserting c would evict b.
	// If Has did NOT promote a, then inserting c evicts a (the older).
	require.False(t, lru.Add(c).Dup)

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

// TestDedupLRU_AddReturnsColdAndEvictedFlags asserts the AddResult
// contract used by the ingest dedup_miss_total counter:
//   - WasEmpty: true iff the LRU had zero entries BEFORE this Add.
//     Reflects the cold-start case where a consumer just restarted
//     with no in-memory history and a redelivery would slip past.
//   - Evicted: true iff this Add forced an eviction of the LRU's oldest
//     entry to make room. Reflects the LRU-saturation case where the
//     evicted id is no longer tracked and a future redelivery of that
//     id would slip past.
//   - Dup: true iff id was already present (true dedup hit).
//
// These signals drive the new sociopulse_analytics_ingest_dedup_miss_total
// counter — incremented when WasEmpty || Evicted on a non-dup add.
func TestDedupLRU_AddReturnsColdAndEvictedFlags(t *testing.T) {
	t.Parallel()

	t.Run("first_add_into_empty_lru_was_empty_true_dup_false", func(t *testing.T) {
		t.Parallel()
		lru := service.NewDedupLRU(4)
		res := lru.Add(uuid.New())
		require.False(t, res.Dup, "first add is not a dup")
		require.True(t, res.WasEmpty, "LRU was empty before first add")
		require.False(t, res.Evicted, "no eviction at Len=0 → Len=1")
	})

	t.Run("subsequent_distinct_add_was_empty_false_evicted_false", func(t *testing.T) {
		t.Parallel()
		lru := service.NewDedupLRU(4)
		_ = lru.Add(uuid.New())
		res := lru.Add(uuid.New())
		require.False(t, res.Dup)
		require.False(t, res.WasEmpty, "LRU had 1 entry before this add")
		require.False(t, res.Evicted, "still room — no eviction")
	})

	t.Run("dup_add_returns_dup_true_was_empty_false", func(t *testing.T) {
		t.Parallel()
		lru := service.NewDedupLRU(4)
		id := uuid.New()
		_ = lru.Add(id)
		res := lru.Add(id) // dup
		require.True(t, res.Dup, "second add of same id is a dup")
		require.False(t, res.WasEmpty, "Len was 1 before second add")
		require.False(t, res.Evicted, "dup add doesn't evict")
	})

	t.Run("eviction_add_returns_evicted_true", func(t *testing.T) {
		t.Parallel()
		lru := service.NewDedupLRU(2)
		_ = lru.Add(uuid.New())
		_ = lru.Add(uuid.New())
		res := lru.Add(uuid.New()) // forces eviction
		require.False(t, res.Dup)
		require.False(t, res.WasEmpty, "Len was at capacity before this add")
		require.True(t, res.Evicted, "capacity-driven eviction must surface")
	})
}

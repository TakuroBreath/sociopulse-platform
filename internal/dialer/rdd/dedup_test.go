package rdd

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newDedupFixture(t *testing.T) (*Dedup, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	d := newDedup(rdb, 1000, 0.01, 30*24*time.Hour)
	return d, mr
}

// TestDedup_FreshPhoneNotSeen — first call to Seen for any phone
// returns false (Bloom miss; Redis SET not consulted).
func TestDedup_FreshPhoneNotSeen(t *testing.T) {
	t.Parallel()
	d, _ := newDedupFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()

	seen, err := d.Seen(context.Background(), tenantID, projectID, "+79161234567")
	require.NoError(t, err)
	require.False(t, seen)
}

// TestDedup_MarkThenSeen — once a phone is Mark'ed, a subsequent Seen
// call returns true.
func TestDedup_MarkThenSeen(t *testing.T) {
	t.Parallel()
	d, _ := newDedupFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()
	phone := "+79161234567"

	require.NoError(t, d.Mark(ctx, tenantID, projectID, phone))
	seen, err := d.Seen(ctx, tenantID, projectID, phone)
	require.NoError(t, err)
	require.True(t, seen)
}

// TestDedup_TenantWideAcrossProjects — within a tenant, both Bloom and
// Redis SET are tenant-scoped; a phone marked in project A is detected
// in project B (and vice versa). This is the intentional dedup
// semantic: an RDD pool draws against the tenant's seen-phones list,
// not project-isolated subsets.
func TestDedup_TenantWideAcrossProjects(t *testing.T) {
	t.Parallel()
	d, _ := newDedupFixture(t)
	tenantID := uuid.New()
	projectA, projectB := uuid.New(), uuid.New()
	ctx := context.Background()
	phone := "+79161234567"

	require.NoError(t, d.Mark(ctx, tenantID, projectA, phone))

	// Same tenant, different project: tenant-scoped Bloom + SET hits.
	seen, err := d.Seen(ctx, tenantID, projectB, phone)
	require.NoError(t, err)
	require.True(t, seen, "tenant-scoped dedup must catch cross-project duplicates")
}

// TestDedup_TenantIsolation — two tenants are fully independent. A
// phone marked in tenant A is invisible to tenant B.
func TestDedup_TenantIsolation(t *testing.T) {
	t.Parallel()
	d, _ := newDedupFixture(t)
	tenantA, tenantB := uuid.New(), uuid.New()
	projectID := uuid.New()
	ctx := context.Background()
	phone := "+79161234567"

	require.NoError(t, d.Mark(ctx, tenantA, projectID, phone))

	seen, err := d.Seen(ctx, tenantB, projectID, phone)
	require.NoError(t, err)
	require.False(t, seen, "tenant B must not see tenant A's phones")
}

// TestDedup_TTLRefresh — Mark refreshes the SET key TTL on every
// invocation.
func TestDedup_TTLRefresh(t *testing.T) {
	t.Parallel()
	d, mr := newDedupFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	require.NoError(t, d.Mark(ctx, tenantID, projectID, "+79161234567"))
	require.Equal(t, 30*24*time.Hour, mr.TTL(d.setKey(tenantID)))
}

// TestDedup_BloomIsLocal — when the Redis SET is somehow purged
// (e.g. TTL elapsed in another process) but the Bloom filter still
// has the entry, Seen confirms via the SET — which now misses — so
// the path is Bloom-hit-then-Redis-miss, returning false.
//
// This is the documented race: Bloom is a cache; Redis SET is
// authoritative. The test pins this behaviour so a future
// implementation that takes Bloom alone as authoritative breaks
// here.
func TestDedup_BloomIsLocal(t *testing.T) {
	t.Parallel()
	d, mr := newDedupFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()
	phone := "+79161234567"

	require.NoError(t, d.Mark(ctx, tenantID, projectID, phone))
	// Simulate the Redis SET being purged (TTL elapsed, manual
	// flush, etc.) WITHOUT touching the in-process Bloom filter.
	mr.FlushDB()

	seen, err := d.Seen(ctx, tenantID, projectID, phone)
	require.NoError(t, err)
	require.False(t, seen, "Bloom alone is NOT authoritative — SET miss must override")
}

// TestDedup_RedisFailureIsErr — closing the client surfaces a
// transport error from Seen / Mark.
func TestDedup_RedisFailureIsErr(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	d := newDedup(rdb, 1000, 0.01, 30*24*time.Hour)

	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()
	phone := "+79161234567"

	// Prime the Bloom so Seen forces a Redis round-trip.
	require.NoError(t, d.Mark(ctx, tenantID, projectID, phone))
	require.NoError(t, rdb.Close())

	_, err := d.Seen(ctx, tenantID, projectID, phone)
	require.Error(t, err)
	require.Contains(t, err.Error(), "dedup")

	require.Error(t, d.Mark(ctx, tenantID, projectID, "+79991234567"))
}

// TestDedup_FilterForConcurrent — concurrent first-touch from
// multiple goroutines for the same tenant produces the SAME
// *bloom.BloomFilter pointer (via the upgrade lock). Run with
// -race; a missing lock would surface here as a data race on the
// internal map.
func TestDedup_FilterForConcurrent(t *testing.T) {
	t.Parallel()
	d, _ := newDedupFixture(t)
	tenantID := uuid.New()

	type bootstrapResult struct {
		ptr reflect.Value
		err error
	}

	const goroutines = 64
	got := make(chan bootstrapResult, goroutines)
	for range goroutines {
		go func() {
			f, err := d.filterFor(context.Background(), tenantID)
			got <- bootstrapResult{ptr: reflect.ValueOf(f), err: err}
		}()
	}
	first := <-got
	require.NoError(t, first.err)
	for range goroutines - 1 {
		next := <-got
		require.NoError(t, next.err)
		require.Equal(t, first.ptr.Pointer(), next.ptr.Pointer(),
			"every concurrent caller must see the same filter pointer")
	}
}

// TestDedup_MarkNReturnsAddCount — MarkN propagates the SADD count
// from the Lua script. First mark of a fresh phone returns 1; second
// mark of the same phone returns 0 (set membership is idempotent).
// This pins the Lua-script return-value contract.
func TestDedup_MarkNReturnsAddCount(t *testing.T) {
	t.Parallel()
	d, _ := newDedupFixture(t)
	tenantID := uuid.New()
	ctx := context.Background()
	phone := "+79161234567"

	added, err := d.MarkN(ctx, tenantID, phone)
	require.NoError(t, err)
	require.EqualValues(t, 1, added, "first mark of a fresh phone must add 1 member")

	added, err = d.MarkN(ctx, tenantID, phone)
	require.NoError(t, err)
	require.EqualValues(t, 0, added, "re-mark of an existing phone must add 0 (SADD-skip)")
}

// TestDedup_BloomBootstrapFromRedis — a fresh Generator process /
// fresh Dedup that points at an existing Redis SET pre-loads the
// in-memory Bloom from the SET so the Seen short-circuit honours
// dedup history written by a peer instance.
func TestDedup_BloomBootstrapFromRedis(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()
	phone := "+79161234567"

	// Seed the Redis SET (no Bloom yet — simulate a peer process /
	// previous run having written this phone).
	require.NoError(t, rdb.SAdd(ctx, "rdd:seen:"+tenantID.String(), phone).Err())

	// Brand-new Dedup. First Seen must bootstrap the Bloom from Redis
	// and report true.
	d := newDedup(rdb, 1000, 0.01, 30*24*time.Hour)
	seen, err := d.Seen(ctx, tenantID, projectID, phone)
	require.NoError(t, err)
	require.True(t, seen, "fresh Dedup must bootstrap from Redis SET")
}

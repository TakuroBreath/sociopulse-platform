package service_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// stubUserResolver counts inner calls; the cache wrapper should
// coalesce concurrent misses into one call and serve subsequent hits
// from the cache for ttl.
type stubUserResolver struct {
	calls atomic.Int64
	mu    sync.Mutex
	data  map[string]string // userID → tenantID
	err   error             // forced error path
}

func newStubUserResolver(data map[string]string) *stubUserResolver {
	return &stubUserResolver{data: data}
}

func (s *stubUserResolver) Get(_ context.Context, userID string) (rtapi.ResolvedTenant, error) {
	s.calls.Add(1)
	if s.err != nil {
		return rtapi.ResolvedTenant{}, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tid, ok := s.data[userID]
	if !ok {
		return rtapi.ResolvedTenant{}, errors.New("not found")
	}
	return rtapi.ResolvedTenant{TenantID: tid}, nil
}

func (s *stubUserResolver) Calls() int64 { return s.calls.Load() }

// TestCachedUserResolver_HitServesFromCache verifies repeated Get
// calls within the TTL hit the cache (one inner call total).
func TestCachedUserResolver_HitServesFromCache(t *testing.T) {
	t.Parallel()

	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	cached := service.NewCachedUserResolver(stub, 60*time.Second)

	for range 5 {
		got, err := cached.Get(t.Context(), "u1")
		require.NoError(t, err)
		assert.Equal(t, "t1", got.TenantID)
	}
	assert.EqualValues(t, 1, stub.Calls(),
		"5 sequential reads of the same key must coalesce to 1 inner call")
}

// TestCachedUserResolver_ExpiryReFetches verifies the TTL: after
// expiry, the next Get hits the inner resolver again.
func TestCachedUserResolver_ExpiryReFetches(t *testing.T) {
	t.Parallel()

	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	// 50ms TTL so the test is fast.
	cached := service.NewCachedUserResolver(stub, 50*time.Millisecond)

	_, err := cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stub.Calls())

	time.Sleep(80 * time.Millisecond)

	_, err = cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 2, stub.Calls(),
		"after TTL elapses, Get must re-query the inner resolver")
}

// slowResolver wraps a UserResolver and delays Get by `delay` —
// used by the singleflight test to force concurrent goroutines to
// overlap inside the inner call.
type slowResolver struct {
	inner *stubUserResolver
	delay time.Duration
}

func (s *slowResolver) Get(ctx context.Context, userID string) (rtapi.ResolvedTenant, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return rtapi.ResolvedTenant{}, ctx.Err()
	}
	return s.inner.Get(ctx, userID)
}

// TestCachedUserResolver_SingleflightCoalescesConcurrentMisses
// drives N goroutines hitting the same uncached key simultaneously.
// The singleflight wrapper must coalesce them into one inner call.
func TestCachedUserResolver_SingleflightCoalescesConcurrentMisses(t *testing.T) {
	t.Parallel()

	const N = 32

	// Slow-down stub: each inner call sleeps 50ms so concurrent
	// callers reliably overlap.
	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	slowStub := &slowResolver{inner: stub, delay: 50 * time.Millisecond}
	cached := service.NewCachedUserResolver(slowStub, 60*time.Second)

	var wg sync.WaitGroup
	for range N {
		wg.Go(func() {
			got, err := cached.Get(t.Context(), "u1")
			assert.NoError(t, err)
			assert.Equal(t, "t1", got.TenantID)
		})
	}
	wg.Wait()

	assert.EqualValues(t, 1, stub.Calls(),
		"singleflight must coalesce N concurrent misses to 1 inner call")
}

// TestCachedUserResolver_CtxCancelPropagates verifies a cancelled ctx
// surfaces ctx.Err() rather than blocking on the inner resolver
// indefinitely.
func TestCachedUserResolver_CtxCancelPropagates(t *testing.T) {
	t.Parallel()

	// Slow inner is short (200ms) so the in-flight closure finishes
	// well before TestMain's goleak.VerifyTestMain runs, even when
	// only the resolver subset of tests is selected. The closure no
	// longer inherits the leader's ctx (Plan 11.2 Task 3 review I-1
	// fix), so the closure runs to completion regardless of the
	// pre-cancelled leader; we only need a delay long enough to
	// guarantee the leader's pre-cancelled ctx wins the outer select.
	stub := &slowResolver{
		inner: newStubUserResolver(map[string]string{"u1": "t1"}),
		delay: 200 * time.Millisecond,
	}
	cached := service.NewCachedUserResolver(stub, 60*time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before call

	_, err := cached.Get(ctx, "u1")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	// Wait for the in-flight closure to drain before the test's
	// cleanup so goleak doesn't flag the still-running slowResolver.
	// The inner finishes at most `delay` after launch; double it for
	// scheduling slack.
	time.Sleep(400 * time.Millisecond)
}

// TestCachedUserResolver_NewWithNilInnerPanics is the wiring guard.
func TestCachedUserResolver_NewWithNilInnerPanics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t,
		"service.NewCachedUserResolver: inner must be non-nil",
		func() {
			_ = service.NewCachedUserResolver(nil, 60*time.Second)
		})
}

// TestCachedUserResolver_LeaderCtxCancelDoesNotPoisonDuplicates is the
// regression guard for the Plan 11.2 Task 3 review IMPORTANT I-1:
// without context.WithoutCancel inside the singleflight closure, a
// leader whose ctx cancels mid-flight would poison every concurrent
// duplicate waiter (singleflight delivers the closure's outcome to
// every chans waiter). Real-world impact: a reconnect storm where
// the first arriving caller's WS drops → every other concurrent
// caller for the same user_id sees context.Canceled.
//
// The fix detaches the closure's ctx from the leader's via
// context.WithoutCancel + a bounded timeout. The outer select on
// ctx.Done() still bounds the leader; the closure survives for the
// duplicates.
func TestCachedUserResolver_LeaderCtxCancelDoesNotPoisonDuplicates(t *testing.T) {
	t.Parallel()

	// Slow inner so the leader's cancel and the duplicate's join
	// reliably overlap.
	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	slow := &slowResolver{inner: stub, delay: 200 * time.Millisecond}
	cached := service.NewCachedUserResolver(slow, 60*time.Second)

	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	type result struct {
		tenant rtapi.ResolvedTenant
		err    error
	}
	leaderRes := make(chan result, 1)
	dupRes := make(chan result, 1)

	// Leader fires first; we wait briefly so the singleflight key is
	// in-flight before the duplicate joins.
	go func() {
		got, err := cached.Get(leaderCtx, "u1")
		leaderRes <- result{got, err}
	}()
	time.Sleep(50 * time.Millisecond)

	// Duplicate joins the in-flight singleflight.
	go func() {
		got, err := cached.Get(t.Context(), "u1") // never-cancelled ctx
		dupRes <- result{got, err}
	}()

	// Cancel ONLY the leader. The duplicate's ctx stays alive.
	time.Sleep(20 * time.Millisecond)
	cancelLeader()

	// Leader observes its own cancellation.
	select {
	case res := <-leaderRes:
		require.ErrorIs(t, res.err, context.Canceled,
			"leader must observe its own context.Canceled")
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not unwind on ctx cancel")
	}

	// CRITICAL: duplicate must get the real result, not the leader's
	// cancelled-ctx error.
	select {
	case res := <-dupRes:
		require.NoError(t, res.err,
			"duplicate waiter must NOT inherit leader's ctx.Canceled")
		assert.Equal(t, "t1", res.tenant.TenantID)
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate did not unwind")
	}

	// Inner was called exactly once (singleflight de-dup preserved).
	assert.EqualValues(t, 1, stub.Calls(),
		"singleflight must coalesce despite leader cancellation")
}

// TestCachedUserResolver_InnerErrorPropagates verifies that when the
// inner resolver returns an error (DB down, RPC timeout, etc.), the
// wrapper does NOT cache the error and propagates it unwrapped to
// the caller. Subsequent calls re-query (no negative caching).
func TestCachedUserResolver_InnerErrorPropagates(t *testing.T) {
	t.Parallel()

	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	stub.err = errors.New("inner: db down")
	cached := service.NewCachedUserResolver(stub, 60*time.Second)

	// First call surfaces the inner error.
	_, err := cached.Get(t.Context(), "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")

	// Subsequent call re-queries (no negative caching).
	_, err = cached.Get(t.Context(), "u1")
	require.Error(t, err)
	assert.EqualValues(t, 2, stub.Calls(),
		"inner errors must NOT be cached; second call must re-query")

	// Clear the error and verify the cache works on success.
	stub.err = nil
	got, err := cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.Equal(t, "t1", got.TenantID)
	assert.EqualValues(t, 3, stub.Calls())

	// Now the success result is cached.
	got, err = cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.Equal(t, "t1", got.TenantID)
	assert.EqualValues(t, 3, stub.Calls(), "post-success calls hit cache")
}

// stubProjectResolver mirrors stubUserResolver for ProjectResolver.
type stubProjectResolver struct {
	calls atomic.Int64
	mu    sync.Mutex
	data  map[string]string
}

func newStubProjectResolver(data map[string]string) *stubProjectResolver {
	return &stubProjectResolver{data: data}
}

func (s *stubProjectResolver) Get(_ context.Context, projectID string) (rtapi.ResolvedTenant, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	tid, ok := s.data[projectID]
	if !ok {
		return rtapi.ResolvedTenant{}, errors.New("not found")
	}
	return rtapi.ResolvedTenant{TenantID: tid}, nil
}

func (s *stubProjectResolver) Calls() int64 { return s.calls.Load() }

// TestCachedProjectResolver_HitServesFromCache mirrors the user
// equivalent for the project port.
func TestCachedProjectResolver_HitServesFromCache(t *testing.T) {
	t.Parallel()

	stub := newStubProjectResolver(map[string]string{"p1": "t1"})
	cached := service.NewCachedProjectResolver(stub, 60*time.Second)

	for range 5 {
		got, err := cached.Get(t.Context(), "p1")
		require.NoError(t, err)
		assert.Equal(t, "t1", got.TenantID)
	}
	assert.EqualValues(t, 1, stub.Calls())
}

// TestCachedProjectResolver_NewWithNilInnerPanics mirrors the user equivalent.
func TestCachedProjectResolver_NewWithNilInnerPanics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t,
		"service.NewCachedProjectResolver: inner must be non-nil",
		func() {
			_ = service.NewCachedProjectResolver(nil, 60*time.Second)
		})
}

// TestCachedUserResolver_InvalidateDropsCachedEntry verifies that
// Invalidate(id) drops the cached entry: the next Get re-queries
// the inner resolver, even within the TTL window.
func TestCachedUserResolver_InvalidateDropsCachedEntry(t *testing.T) {
	t.Parallel()

	stub := newStubUserResolver(map[string]string{"u1": "t1"})
	cached := service.NewCachedUserResolver(stub, 60*time.Second)

	// First Get caches the entry.
	_, err := cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stub.Calls())

	// Second Get hits the cache (no new inner call).
	_, err = cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stub.Calls())

	// Invalidate the entry.
	cached.Invalidate("u1")

	// Next Get must re-query the inner resolver.
	_, err = cached.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.EqualValues(t, 2, stub.Calls(),
		"Get after Invalidate must re-query the inner resolver")
}

// TestCachedUserResolver_InvalidateUnknownIDIsNoop verifies that
// Invalidate on an ID that was never cached is a silent no-op.
// (The singleflight.Forget on a missing key is also a no-op per
// the upstream documentation; the test locks in the contract.)
func TestCachedUserResolver_InvalidateUnknownIDIsNoop(t *testing.T) {
	t.Parallel()

	stub := newStubUserResolver(map[string]string{})
	cached := service.NewCachedUserResolver(stub, 60*time.Second)

	require.NotPanics(t, func() {
		cached.Invalidate("never-cached")
	})

	// Invalidate must not touch the inner resolver — Forget on a
	// missing singleflight key is a no-op, Delete on a missing
	// sync.Map key is a no-op.
	assert.EqualValues(t, 0, stub.Calls(),
		"Invalidate on unknown ID must not call inner resolver")
}

// TestCachedProjectResolver_InvalidateDropsCachedEntry mirrors
// the user equivalent for the project port.
func TestCachedProjectResolver_InvalidateDropsCachedEntry(t *testing.T) {
	t.Parallel()

	stub := newStubProjectResolver(map[string]string{"p1": "t1"})
	cached := service.NewCachedProjectResolver(stub, 60*time.Second)

	_, err := cached.Get(t.Context(), "p1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stub.Calls())

	cached.Invalidate("p1")

	_, err = cached.Get(t.Context(), "p1")
	require.NoError(t, err)
	assert.EqualValues(t, 2, stub.Calls(),
		"Get after Invalidate must re-query the inner resolver")
}

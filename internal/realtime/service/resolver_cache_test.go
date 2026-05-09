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
	"go.uber.org/zap/zaptest"

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
	cached := service.NewCachedUserResolver(stub, 60*time.Second, zaptest.NewLogger(t))

	for i := 0; i < 5; i++ {
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
	cached := service.NewCachedUserResolver(stub, 50*time.Millisecond, zaptest.NewLogger(t))

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
	cached := service.NewCachedUserResolver(slowStub, 60*time.Second, zaptest.NewLogger(t))

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cached.Get(t.Context(), "u1")
			assert.NoError(t, err)
			assert.Equal(t, "t1", got.TenantID)
		}()
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

	stub := &slowResolver{
		inner: newStubUserResolver(map[string]string{"u1": "t1"}),
		delay: 5 * time.Second,
	}
	cached := service.NewCachedUserResolver(stub, 60*time.Second, zaptest.NewLogger(t))

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before call

	_, err := cached.Get(ctx, "u1")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestCachedUserResolver_NewWithNilInnerPanics is the wiring guard.
func TestCachedUserResolver_NewWithNilInnerPanics(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t,
		"service.NewCachedUserResolver: inner must be non-nil",
		func() {
			_ = service.NewCachedUserResolver(nil, 60*time.Second, zaptest.NewLogger(t))
		})
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
	cached := service.NewCachedProjectResolver(stub, 60*time.Second, zaptest.NewLogger(t))

	for i := 0; i < 5; i++ {
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
			_ = service.NewCachedProjectResolver(nil, 60*time.Second, zaptest.NewLogger(t))
		})
}

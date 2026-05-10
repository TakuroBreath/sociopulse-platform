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

// fakeCallResolver counts inner calls so cache-hit assertions can
// verify the wrapper's coalescing.
type fakeCallResolver struct {
	mu     sync.Mutex
	calls  atomic.Int64
	want   map[string]rtapi.ResolvedTenant
	errFor map[string]error
	delay  time.Duration
}

func (f *fakeCallResolver) Get(ctx context.Context, callID string) (rtapi.ResolvedTenant, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return rtapi.ResolvedTenant{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errFor != nil {
		if err, ok := f.errFor[callID]; ok {
			return rtapi.ResolvedTenant{}, err
		}
	}
	return f.want[callID], nil
}

// TestCachedCallResolver_HitDoesNotCallInner verifies the cache hit
// path returns the cached entry without re-querying the inner resolver.
func TestCachedCallResolver_HitDoesNotCallInner(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{
		want: map[string]rtapi.ResolvedTenant{"call-1": {TenantID: "t-1"}},
	}
	c := service.NewCachedCallResolver(inner, 0)

	got, err := c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, "t-1", got.TenantID)
	require.Equal(t, int64(1), inner.calls.Load())

	// Second Get within ttl returns the cached value without an
	// inner call.
	got, err = c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, "t-1", got.TenantID)
	require.Equal(t, int64(1), inner.calls.Load(), "cache hit must not re-query inner")
}

// TestCachedCallResolver_TTLExpiry verifies that an expired entry
// triggers a re-fetch of the inner resolver.
func TestCachedCallResolver_TTLExpiry(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{
		want: map[string]rtapi.ResolvedTenant{"call-1": {TenantID: "t-1"}},
	}
	// 50ms ttl so the test isn't slow.
	c := service.NewCachedCallResolver(inner, 50*time.Millisecond)

	_, err := c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, int64(1), inner.calls.Load())

	time.Sleep(60 * time.Millisecond)

	_, err = c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, int64(2), inner.calls.Load(),
		"expired entry must re-query inner")
}

// TestCachedCallResolver_InnerError surfaces inner-resolver errors and
// does NOT cache the failure (matching CachedUserResolver/Project).
func TestCachedCallResolver_InnerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("call not found")
	inner := &fakeCallResolver{
		want:   map[string]rtapi.ResolvedTenant{},
		errFor: map[string]error{"call-x": wantErr},
	}
	c := service.NewCachedCallResolver(inner, 0)

	_, err := c.Get(t.Context(), "call-x")
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, int64(1), inner.calls.Load())

	// A second Get must re-query — no negative caching.
	_, err = c.Get(t.Context(), "call-x")
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, int64(2), inner.calls.Load(),
		"errors must not be cached")
}

// TestCachedCallResolver_Invalidate drops the cached entry so the next
// Get re-queries the inner resolver.
func TestCachedCallResolver_Invalidate(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{
		want: map[string]rtapi.ResolvedTenant{"call-1": {TenantID: "t-1"}},
	}
	c := service.NewCachedCallResolver(inner, 0)

	_, err := c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, int64(1), inner.calls.Load())

	c.Invalidate("call-1")

	_, err = c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, int64(2), inner.calls.Load(),
		"Invalidate must drop the cached entry")
}

// TestCachedCallResolver_Invalidate_Idempotent — Invalidate on a
// missing key is a no-op.
func TestCachedCallResolver_Invalidate_Idempotent(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{want: map[string]rtapi.ResolvedTenant{}}
	c := service.NewCachedCallResolver(inner, 0)

	assert.NotPanics(t, func() { c.Invalidate("never-cached") })
}

// TestCachedCallResolver_LeaderCtxCancelDoesNotPoisonDuplicates is the
// ctx-bleed regression guard. A leader whose ctx cancels MUST NOT
// poison concurrent waiters joining the in-flight singleflight call.
// See Plan 11.2 Task 3 review IMPORTANT I-1.
func TestCachedCallResolver_LeaderCtxCancelDoesNotPoisonDuplicates(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{
		want:  map[string]rtapi.ResolvedTenant{"call-1": {TenantID: "t-1"}},
		delay: 100 * time.Millisecond,
	}
	c := service.NewCachedCallResolver(inner, 0)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	dupCtx := context.Background()

	leaderResult := make(chan error, 1)
	dupResult := make(chan struct {
		v   rtapi.ResolvedTenant
		err error
	}, 1)

	go func() {
		_, err := c.Get(leaderCtx, "call-1")
		leaderResult <- err
	}()
	// Let leader register its singleflight key.
	time.Sleep(20 * time.Millisecond)
	go func() {
		v, err := c.Get(dupCtx, "call-1")
		dupResult <- struct {
			v   rtapi.ResolvedTenant
			err error
		}{v, err}
	}()

	// Cancel the leader's ctx — the duplicate must still receive the
	// real result via the detached inner ctx.
	time.Sleep(20 * time.Millisecond)
	cancelLeader()

	leaderErr := <-leaderResult
	dup := <-dupResult

	require.ErrorIs(t, leaderErr, context.Canceled, "leader sees its own ctx-cancel")
	require.NoError(t, dup.err, "duplicate must NOT inherit leader's ctx-cancel")
	require.Equal(t, "t-1", dup.v.TenantID)
}

// TestCachedCallResolver_NewWithNilInnerPanics is the wiring guard.
func TestCachedCallResolver_NewWithNilInnerPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { _ = service.NewCachedCallResolver(nil, 0) })
}

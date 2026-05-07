package passwords_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/passwords"
)

// gateHasher is a Hasher fake whose Hash blocks until release is signaled.
// Used to drive concurrency tests without paying real Argon2 cost.
type gateHasher struct {
	release     chan struct{}
	releaseOnce sync.Once
	inFlight    int64 // atomic
	maxSeen     int64 // atomic
}

func newGateHasher() *gateHasher {
	return &gateHasher{release: make(chan struct{})}
}

func (g *gateHasher) Hash(ctx context.Context, _ string) (string, error) {
	cur := atomic.AddInt64(&g.inFlight, 1)
	for {
		seen := atomic.LoadInt64(&g.maxSeen)
		if cur <= seen || atomic.CompareAndSwapInt64(&g.maxSeen, seen, cur) {
			break
		}
	}
	defer atomic.AddInt64(&g.inFlight, -1)

	select {
	case <-g.release:
		return "stub-hash", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (g *gateHasher) Verify(ctx context.Context, _, _ string) (bool, error) {
	select {
	case <-g.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Release unblocks all in-flight callers. Safe to call multiple times so
// tests can both release explicitly and register t.Cleanup(Release) for
// safety on early failure.
func (g *gateHasher) Release() { g.releaseOnce.Do(func() { close(g.release) }) }

// fastHasher is a Hasher fake whose Hash returns immediately. Used for
// behavior tests that don't care about concurrency.
type fastHasher struct {
	hashCalls   int64
	verifyCalls int64
}

func (f *fastHasher) Hash(_ context.Context, _ string) (string, error) {
	atomic.AddInt64(&f.hashCalls, 1)
	return "fast-hash", nil
}

func (f *fastHasher) Verify(_ context.Context, _, _ string) (bool, error) {
	atomic.AddInt64(&f.verifyCalls, 1)
	return true, nil
}

func TestBoundedHasher_DelegatesHashAndVerify(t *testing.T) {
	t.Parallel()

	inner := &fastHasher{}
	bh := passwords.NewBoundedHasher(inner, 4)

	got, err := bh.Hash(context.Background(), "x")
	require.NoError(t, err)
	assert.Equal(t, "fast-hash", got)

	ok, err := bh.Verify(context.Background(), "any-hash", "x")
	require.NoError(t, err)
	assert.True(t, ok)

	assert.Equal(t, int64(1), atomic.LoadInt64(&inner.hashCalls))
	assert.Equal(t, int64(1), atomic.LoadInt64(&inner.verifyCalls))
}

func TestBoundedHasher_LimitsConcurrencyToMaxConcurrent(t *testing.T) {
	t.Parallel()

	const maxConcurrent = 3
	const totalCallers = 10

	inner := newGateHasher()
	bh := passwords.NewBoundedHasher(inner, maxConcurrent)
	// Guarantee the goroutines unblock even if the test fails partway
	// through — otherwise we'd leak workers across the suite.
	t.Cleanup(inner.Release)

	// Launch totalCallers goroutines all racing into Hash. Only maxConcurrent
	// should ever be inside inner.Hash at once.
	var wg sync.WaitGroup
	wg.Add(totalCallers)
	for range totalCallers {
		go func() {
			defer wg.Done()
			_, _ = bh.Hash(context.Background(), "x")
		}()
	}

	// Block until exactly maxConcurrent callers have entered inner.Hash.
	// This is deterministic — no fixed sleep — so a slow CI runner cannot
	// false-fail the assertion below.
	require.Eventually(t, func() bool {
		return atomic.LoadInt64(&inner.inFlight) == int64(maxConcurrent)
	}, time.Second, time.Millisecond,
		"expected exactly %d in-flight goroutines", maxConcurrent)

	assert.LessOrEqual(t, atomic.LoadInt64(&inner.maxSeen), int64(maxConcurrent),
		"never more than maxConcurrent in-flight")

	inner.Release()
	wg.Wait()
}

func TestBoundedHasher_RespectsContextDeadline(t *testing.T) {
	t.Parallel()

	inner := newGateHasher()
	bh := passwords.NewBoundedHasher(inner, 1)

	// Saturate the single slot.
	saturated := make(chan struct{})
	go func() {
		_, _ = bh.Hash(context.Background(), "x")
		close(saturated)
	}()

	// Wait until inner is in-flight so the next caller has to queue.
	require.Eventually(t, func() bool {
		return atomic.LoadInt64(&inner.inFlight) == 1
	}, time.Second, time.Millisecond)

	// A second caller with a tight deadline must give up.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	_, err := bh.Hash(ctx, "x")
	require.Error(t, err)
	assert.ErrorIs(t, err, passwords.ErrHasherBusy,
		"deadline-expired waiters must surface ErrHasherBusy")
	// The original deadline error must remain reachable too — this is the
	// documented dual-unwrap contract (handlers want both: ErrHasherBusy
	// to know "saturated, retry later" AND DeadlineExceeded to know the
	// specific reason for telemetry).
	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"deadline context error must remain reachable via errors.Is")

	// Cleanup: release the gate so the saturating goroutine can exit.
	inner.Release()
	<-saturated
}

func TestBoundedHasher_PropagatesInnerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("inner failed")
	inner := errHasher{err: wantErr}
	bh := passwords.NewBoundedHasher(inner, 4)

	// Hash path
	_, err := bh.Hash(context.Background(), "x")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr,
		"Hash wrapping must preserve errors.Is on the inner error")
	assert.NotErrorIs(t, err, passwords.ErrHasherBusy,
		"unrelated inner errors must NOT look like a busy signal")

	// Verify path — same contract.
	_, verr := bh.Verify(context.Background(), "encoded", "x")
	require.Error(t, verr)
	assert.ErrorIs(t, verr, wantErr, "Verify must propagate inner error too")
	assert.NotErrorIs(t, verr, passwords.ErrHasherBusy)
}

func TestBoundedHasher_ReleasesSlotOnInnerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("inner failed")
	inner := errHasher{err: wantErr}
	bh := passwords.NewBoundedHasher(inner, 1)

	for range 5 {
		_, err := bh.Hash(context.Background(), "x")
		require.ErrorIs(t, err, wantErr)
	}
	// If the slot wasn't released after each error, the 6th call would
	// deadlock here. The test passing IS the assertion.
	_, err := bh.Hash(context.Background(), "x")
	require.ErrorIs(t, err, wantErr)
}

func TestNewBoundedHasher_NormalizesNonPositiveLimit(t *testing.T) {
	t.Parallel()

	// Zero or negative ceiling is a programmer mistake; we coerce to 1
	// rather than wedge — calling code that passes runtime.NumCPU() on
	// platforms reporting 0 still gets a working hasher.
	for _, limit := range []int{0, -1, -100} {
		bh := passwords.NewBoundedHasher(&fastHasher{}, limit)
		_, err := bh.Hash(context.Background(), "x")
		require.NoError(t, err, "limit=%d must not break", limit)
	}
}

// errHasher is a Hasher whose every method returns the configured error.
type errHasher struct{ err error }

func (e errHasher) Hash(_ context.Context, _ string) (string, error) {
	return "", e.err
}

func (e errHasher) Verify(_ context.Context, _, _ string) (bool, error) {
	return false, e.err
}

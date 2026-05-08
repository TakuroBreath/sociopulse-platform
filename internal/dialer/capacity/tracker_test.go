package capacity_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/capacity"
)

// TestMain lives in main_test.go (non-integration build). The
// integration build sources its own TestMain from
// tracker_integration_test.go (which is `//go:build integration`-tagged
// and would otherwise collide with the default-build TestMain in this
// file). Go disallows two TestMain in one test package.

// fakePool is a recording capacity.Pool used to drive the Tracker
// against a known healthy-node list.
type fakePool struct {
	mu    sync.Mutex
	nodes []string
}

func (f *fakePool) HealthyNodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.nodes))
	copy(out, f.nodes)
	return out
}

func (f *fakePool) setNodes(ns []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes = append([]string(nil), ns...)
}

// fakeBackpressure is an in-memory capacity.Bp implementation. Each
// node has a per-node cap (default capPerNode) and an INCR/DECR-bounded
// counter. It also supports per-method error injection so tests can
// assert the Tracker's transport-error and skip-and-continue paths.
type fakeBackpressure struct {
	mu sync.Mutex

	cap        int
	counters   map[string]int
	tryAcqErr  map[string]error // node → error returned by TryAcquire
	releaseErr map[string]error
	getErr     map[string]error

	// alwaysFull marks nodes whose TryAcquire returns (false, nil)
	// regardless of the underlying counter. Lets tests force the
	// "all-full" walk paths deterministically without having to
	// preload the counter to cap.
	alwaysFull map[string]bool

	tryAcquireCount int64 // total TryAcquire calls — exposes round-robin walk
}

func newFakeBackpressure() *fakeBackpressure {
	return &fakeBackpressure{
		cap:        newRigCap,
		counters:   make(map[string]int),
		tryAcqErr:  make(map[string]error),
		releaseErr: make(map[string]error),
		getErr:     make(map[string]error),
		alwaysFull: make(map[string]bool),
	}
}

func (f *fakeBackpressure) TryAcquire(_ context.Context, node string) (bool, error) {
	atomic.AddInt64(&f.tryAcquireCount, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.tryAcqErr[node]; ok && err != nil {
		return false, err
	}
	if f.alwaysFull[node] {
		return false, nil
	}
	if f.counters[node] >= f.cap {
		return false, nil
	}
	f.counters[node]++
	return true, nil
}

func (f *fakeBackpressure) Release(_ context.Context, node string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.releaseErr[node]; ok && err != nil {
		return err
	}
	if f.counters[node] > 0 {
		f.counters[node]--
	}
	return nil
}

func (f *fakeBackpressure) Get(_ context.Context, node string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.getErr[node]; ok && err != nil {
		return 0, err
	}
	return f.counters[node], nil
}

func (f *fakeBackpressure) Cap() int { return f.cap }

// markFull forces TryAcquire on node to return (false, nil) — the
// "node at cap" signal — regardless of the underlying counter.
func (f *fakeBackpressure) markFull(node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alwaysFull[node] = true
}

func (f *fakeBackpressure) setTryAcquireErr(node string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tryAcqErr[node] = err
}

func (f *fakeBackpressure) setReleaseErr(node string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseErr[node] = err
}

func (f *fakeBackpressure) setGetErr(node string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr[node] = err
}

func (f *fakeBackpressure) counter(node string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counters[node]
}

// rig wires up Tracker with fakes + fresh metrics.
type rig struct {
	t       *capacity.Tracker
	pool    *fakePool
	bp      *fakeBackpressure
	metrics *capacity.Metrics
}

// newRigCap is the canonical per-node cap used by every fake-backed
// unit test. Real production cap is 60 and integration tests exercise
// smaller caps to drive saturation paths cheaply; the unit tests do
// the same except they only need ONE value and don't benefit from the
// per-test parameter.
const newRigCap = 60

func newRig(t *testing.T, nodes []string) *rig {
	t.Helper()
	pool := &fakePool{}
	pool.setNodes(nodes)
	bp := newFakeBackpressure()
	reg := prometheus.NewRegistry()
	metrics := capacity.RegisterMetrics(reg)
	tr, err := capacity.New(capacity.Config{
		Pool:         pool,
		Backpressure: bp,
		Logger:       zaptest.NewLogger(t),
		Metrics:      metrics,
	})
	require.NoError(t, err)
	return &rig{t: tr, pool: pool, bp: bp, metrics: metrics}
}

// Compile-time interface assertions — fakes satisfy the package's
// declared dependency surface. If those drift the test package fails to
// compile, surfacing the breakage at the same checkpoint as the
// production assertions in tracker.go.
var (
	_ capacity.Pool = (*fakePool)(nil)
	_ capacity.Bp   = (*fakeBackpressure)(nil)
)

// TestNew_RequiresPool — Pool is required.
func TestNew_RequiresPool(t *testing.T) {
	t.Parallel()
	_, err := capacity.New(capacity.Config{
		Backpressure: newFakeBackpressure(),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Pool")
}

// TestNew_RequiresBackpressure — Backpressure is required.
func TestNew_RequiresBackpressure(t *testing.T) {
	t.Parallel()
	_, err := capacity.New(capacity.Config{
		Pool: &fakePool{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Backpressure")
}

// TestNew_Defaults — nil Logger / Metrics fall back; constructor returns
// no error.
func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	_, err := capacity.New(capacity.Config{
		Pool:         &fakePool{},
		Backpressure: newFakeBackpressure(),
	})
	require.NoError(t, err)
}

// TestAcquire_HappyPath — three healthy nodes, every TryAcquire returns
// ok=true; Tracker returns one of them and the result counter ticks.
func TestAcquire_HappyPath(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"fs1", "fs2", "fs3"})

	node, err := r.t.Acquire(context.Background())
	require.NoError(t, err)
	require.Contains(t, []string{"fs1", "fs2", "fs3"}, node)

	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Acquires.WithLabelValues("ok")), 0)
	require.InDelta(t, 0.0, testutil.ToFloat64(r.metrics.Acquires.WithLabelValues("all_full")), 0)
	// Active gauge for the chosen node should reflect the new counter.
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Active.WithLabelValues(node)), 0)
}

// TestAcquire_AllNodesFull — every node returns ok=false → Tracker
// returns api.ErrAllNodesFull and ticks the all_full counter.
func TestAcquire_AllNodesFull(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"fs1", "fs2", "fs3"})
	r.bp.markFull("fs1")
	r.bp.markFull("fs2")
	r.bp.markFull("fs3")

	node, err := r.t.Acquire(context.Background())
	require.ErrorIs(t, err, api.ErrAllNodesFull)
	require.Empty(t, node)
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Acquires.WithLabelValues("all_full")), 0)
	// And TryAcquire was attempted on EVERY node before giving up.
	require.Equal(t, int64(3), atomic.LoadInt64(&r.bp.tryAcquireCount))
}

// TestAcquire_RoundRobin — three successive Acquires on a 3-node fleet
// must rotate through ALL 3 nodes (counter advances). The starting
// offset is taken from atomic.Uint64.Add - 1 so the FIRST call's
// starting index advances the counter by one each call.
func TestAcquire_RoundRobin(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"a", "b", "c"})

	seen := make(map[string]struct{}, 3)
	for range 3 {
		node, err := r.t.Acquire(context.Background())
		require.NoError(t, err)
		seen[node] = struct{}{}
	}
	// All three healthy nodes should have been observed at least once.
	require.Len(t, seen, 3, "round-robin must rotate across every node within len(nodes) Acquires")
}

// TestAcquire_PoolEmpty — HealthyNodes returns nil → Tracker returns
// api.ErrAllNodesFull immediately (without any TryAcquire call).
func TestAcquire_PoolEmpty(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)

	node, err := r.t.Acquire(context.Background())
	require.ErrorIs(t, err, api.ErrAllNodesFull)
	require.Empty(t, node)
	require.Equal(t, int64(0), atomic.LoadInt64(&r.bp.tryAcquireCount),
		"pool-empty path must NOT touch Backpressure")
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Acquires.WithLabelValues("all_full")), 0)
}

// TestAcquire_SkipsErroringNode — TryAcquire on the first picked node
// returns a transport error; the Tracker logs + skips and tries the
// next. The eventual success is recorded under the ok label.
func TestAcquire_SkipsErroringNode(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"a", "b"})
	transport := errors.New("redis: connection refused")
	// Force the FIRST node visited to error. Round-robin starts the
	// walk at counter+1 mod len; counter is fresh so first call starts
	// at index 0 → "a". Setting an error on "a" exercises the skip.
	r.bp.setTryAcquireErr("a", transport)

	node, err := r.t.Acquire(context.Background())
	require.NoError(t, err)
	require.Equal(t, "b", node, "Tracker must skip past erroring node 'a' to 'b'")
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Acquires.WithLabelValues("ok")), 0)
}

// TestAcquire_AllNodesError — every node errors → Tracker returns
// api.ErrAllNodesFull (the "exhausted" sentinel). The "error"
// metric label ticks once per per-node error so operators can see the
// attempted-and-failed count separate from the all_full bucket.
func TestAcquire_AllNodesError(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"a", "b"})
	transport := errors.New("redis down")
	r.bp.setTryAcquireErr("a", transport)
	r.bp.setTryAcquireErr("b", transport)

	node, err := r.t.Acquire(context.Background())
	require.ErrorIs(t, err, api.ErrAllNodesFull)
	require.Empty(t, node)
	// Every node was attempted.
	require.Equal(t, int64(2), atomic.LoadInt64(&r.bp.tryAcquireCount))
	// All_full ticks once (the terminal result); error ticks twice
	// (once per per-node failure).
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Acquires.WithLabelValues("all_full")), 0)
	require.InDelta(t, 2.0, testutil.ToFloat64(r.metrics.Acquires.WithLabelValues("error")), 0)
}

// TestRelease_Passthrough — Release forwards to Backpressure.Release
// for the named node and ticks the ok counter.
func TestRelease_Passthrough(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"fs1"})
	// Pre-load the counter via TryAcquire so Release has something to
	// decrement.
	_, _ = r.bp.TryAcquire(context.Background(), "fs1")
	require.Equal(t, 1, r.bp.counter("fs1"))

	require.NoError(t, r.t.Release(context.Background(), "fs1"))
	require.Equal(t, 0, r.bp.counter("fs1"),
		"Release must decrement the underlying Backpressure counter")
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Releases.WithLabelValues("ok")), 0)
	// Active gauge reflects the post-release counter.
	require.InDelta(t, 0.0, testutil.ToFloat64(r.metrics.Active.WithLabelValues("fs1")), 0)
}

// TestRelease_PropagatesError — Release's error path forwards the
// underlying transport error verbatim and ticks the error counter.
func TestRelease_PropagatesError(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"fs1"})
	transport := errors.New("redis down")
	r.bp.setReleaseErr("fs1", transport)

	err := r.t.Release(context.Background(), "fs1")
	require.ErrorIs(t, err, transport)
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Releases.WithLabelValues("error")), 0)
}

// TestRelease_RejectsEmptyNode — defensive: Release with empty node is
// a wiring bug from the caller and surfaces as an error rather than
// silently no-op.
func TestRelease_RejectsEmptyNode(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"fs1"})
	err := r.t.Release(context.Background(), "")
	require.Error(t, err)
}

// TestStats_HappyPath — Stats builds a map keyed by the healthy nodes,
// values from Backpressure.Get for each.
func TestStats_HappyPath(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"fs1", "fs2"})
	// Pre-load distinct counters per node.
	_, _ = r.bp.TryAcquire(context.Background(), "fs1")
	_, _ = r.bp.TryAcquire(context.Background(), "fs1")
	_, _ = r.bp.TryAcquire(context.Background(), "fs2")

	stats, err := r.t.Stats(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), stats["fs1"])
	require.Equal(t, int64(1), stats["fs2"])
	require.Len(t, stats, 2)
	// Active gauge is updated for every node Stats observed.
	require.InDelta(t, 2.0, testutil.ToFloat64(r.metrics.Active.WithLabelValues("fs1")), 0)
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Active.WithLabelValues("fs2")), 0)
}

// TestStats_PoolEmpty — no healthy nodes → empty map (not nil), no
// error.
func TestStats_PoolEmpty(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	stats, err := r.t.Stats(context.Background())
	require.NoError(t, err)
	require.NotNil(t, stats)
	require.Empty(t, stats)
}

// TestStats_SkipsErroringNode — one node's Get errors; others are still
// reported. The whole call does NOT fail. This is the production
// degraded-mode contract: a single Redis hiccup on one node must not
// drop the entire stats payload.
func TestStats_SkipsErroringNode(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"fs1", "fs2", "fs3"})
	_, _ = r.bp.TryAcquire(context.Background(), "fs1")
	_, _ = r.bp.TryAcquire(context.Background(), "fs3")
	r.bp.setGetErr("fs2", errors.New("redis: timeout"))

	stats, err := r.t.Stats(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), stats["fs1"])
	require.Equal(t, int64(1), stats["fs3"])
	_, ok := stats["fs2"]
	require.False(t, ok, "erroring node must be skipped, not zero-valued")
	require.Len(t, stats, 2)
}

// TestNilMetricsTolerated — building a Tracker without metrics is
// supported; every observe path no-ops. Belt-and-braces against a
// future regression that adds a non-nil-checked metric tick.
func TestNilMetricsTolerated(t *testing.T) {
	t.Parallel()
	pool := &fakePool{}
	pool.setNodes([]string{"fs1"})
	bp := newFakeBackpressure()
	tr, err := capacity.New(capacity.Config{
		Pool:         pool,
		Backpressure: bp,
		Logger:       zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	// Acquire / Release / Stats must not panic with nil metrics.
	node, err := tr.Acquire(context.Background())
	require.NoError(t, err)
	require.NoError(t, tr.Release(context.Background(), node))

	stats, err := tr.Stats(context.Background())
	require.NoError(t, err)
	require.NotNil(t, stats)

	// Drive the all_full and error metric branches under nil metrics.
	bp.markFull("fs1")
	_, err = tr.Acquire(context.Background())
	require.ErrorIs(t, err, api.ErrAllNodesFull)
}

// TestNilLoggerTolerated — building a Tracker without a logger uses
// zap.NewNop and every method continues to work.
func TestNilLoggerTolerated(t *testing.T) {
	t.Parallel()
	pool := &fakePool{}
	pool.setNodes([]string{"fs1"})
	bp := newFakeBackpressure()
	tr, err := capacity.New(capacity.Config{
		Pool:         pool,
		Backpressure: bp,
	})
	require.NoError(t, err)

	// Drive the skip-and-continue path so the nil-log call site fires.
	bp.setTryAcquireErr("fs1", errors.New("redis err"))
	_, err = tr.Acquire(context.Background())
	require.ErrorIs(t, err, api.ErrAllNodesFull)
}

// TestRegisterMetricsNilRegistererPanics — the contract is "panic on
// nil reg" (matches FSM/queue/RDD/router packages).
func TestRegisterMetricsNilRegistererPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { capacity.RegisterMetrics(nil) })
}

// TestAcquire_RoundRobinFairness — under sustained partial load (e.g.
// caps reached on the first 2 of 4 nodes), the round-robin still
// distributes across the REMAINING healthy nodes. Asserts the second
// invariant of the round-robin policy: skip-on-full does not pin to a
// single node.
func TestAcquire_RoundRobinFairness(t *testing.T) {
	t.Parallel()
	r := newRig(t, []string{"a", "b", "c", "d"})
	// Force a and b to always be at cap; c and d are open.
	r.bp.markFull("a")
	r.bp.markFull("b")

	const calls = 20
	picks := make(map[string]int, 2)
	for range calls {
		node, err := r.t.Acquire(context.Background())
		require.NoError(t, err)
		picks[node]++
	}

	// All picks fell on c or d; neither was starved.
	require.Equal(t, calls, picks["c"]+picks["d"])
	require.Positive(t, picks["c"])
	require.Positive(t, picks["d"])
}

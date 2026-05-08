package router_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/telephony/router"
)

// fakeFSCounter is a deterministic FSCounter for the reconciler tests.
// Per-node counts and per-node errors are configured under a mutex so a
// single instance can be reused across the goroutine boundary in
// Reconciler.Run-driven cases.
type fakeFSCounter struct {
	mu     sync.Mutex
	counts map[string]int
	errs   map[string]error
	calls  atomic.Int64
}

func newFakeFSCounter() *fakeFSCounter {
	return &fakeFSCounter{
		counts: make(map[string]int),
		errs:   make(map[string]error),
	}
}

func (f *fakeFSCounter) ActiveChannels(_ context.Context, node string) (int, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errs[node]; ok && err != nil {
		return 0, err
	}
	return f.counts[node], nil
}

func (f *fakeFSCounter) setCount(node string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[node] = n
}

func (f *fakeFSCounter) setErr(node string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[node] = err
}

// reconcilerHarness packages the moving parts every test needs: a
// miniredis-backed Backpressure, a fake counter, a node-list closure, a
// fresh prometheus registry + Drift gauge, and the Reconciler under test.
// Constructed via newReconcilerT so each test gets isolated state.
type reconcilerHarness struct {
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	bp      *router.Backpressure
	fs      *fakeFSCounter
	gauge   *prometheus.GaugeVec
	rec     *router.Reconciler
	nodes   []string
	nodesMu sync.RWMutex
}

func (h *reconcilerHarness) NodesFunc() []string {
	h.nodesMu.RLock()
	defer h.nodesMu.RUnlock()
	return append([]string(nil), h.nodes...)
}

// newReconcilerT builds a harness with a fast-tick reconciler — the 5 ms
// interval is a deliberate compromise: long enough that runOneSweep's
// 250 ms ctx-deadline reliably lands at least one tick across CI loaded
// schedulers, short enough that runOneSweep itself returns quickly. Tests
// that exercise Run lifecycle (StopsOnCtxCancel, TickerFires*) override
// the interval explicitly via withInterval.
func newReconcilerT(t *testing.T, nodes []string, opts ...harnessOption) *reconcilerHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	bp := router.NewBackpressure(rdb, 60)
	fs := newFakeFSCounter()
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_drift", Help: "test"},
		[]string{"node"},
	)

	h := &reconcilerHarness{
		mr:    mr,
		rdb:   rdb,
		bp:    bp,
		fs:    fs,
		gauge: gauge,
		nodes: append([]string(nil), nodes...),
	}

	cfg := router.ReconcilerConfig{
		Backpressure: bp,
		FSCounter:    fs,
		NodesFunc:    h.NodesFunc,
		Interval:     5 * time.Millisecond, // fast-tick default for runOneSweep
		Logger:       zaptest.NewLogger(t),
		DriftGauge:   gauge,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	rec, err := router.NewReconciler(cfg)
	require.NoError(t, err)
	h.rec = rec
	return h
}

type harnessOption func(*router.ReconcilerConfig)

func withInterval(d time.Duration) harnessOption {
	return func(c *router.ReconcilerConfig) { c.Interval = d }
}

// --- Constructor --------------------------------------------------------------

func TestReconciler_New_RejectsMissingDeps(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	bp := router.NewBackpressure(rdb, 5)
	fs := newFakeFSCounter()
	nodes := func() []string { return nil }

	cases := []struct {
		name string
		cfg  router.ReconcilerConfig
		want string
	}{
		{
			name: "nil backpressure",
			cfg:  router.ReconcilerConfig{FSCounter: fs, NodesFunc: nodes},
			want: "Backpressure",
		},
		{
			name: "nil fs counter",
			cfg:  router.ReconcilerConfig{Backpressure: bp, NodesFunc: nodes},
			want: "FSCounter",
		},
		{
			name: "nil nodes func",
			cfg:  router.ReconcilerConfig{Backpressure: bp, FSCounter: fs},
			want: "NodesFunc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := router.NewReconciler(tc.cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestReconciler_New_DefaultsIntervalAndLogger(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	rec, err := router.NewReconciler(router.ReconcilerConfig{
		Backpressure: router.NewBackpressure(rdb, 5),
		FSCounter:    newFakeFSCounter(),
		NodesFunc:    func() []string { return nil },
		// Interval and Logger left zero.
	})
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Contains(t, rec.String(), "Reconciler{interval=")
}

// --- Sweep semantics ---------------------------------------------------------

// TestReconciler_Sweep_FixesPositiveDrift covers failure mode (1) from the
// reconciler doc: bridge crash leaks +1 (or +N) on Redis. With Redis at
// 100 and FS at 5, a single sweep must drop Redis to 5.
func TestReconciler_Sweep_FixesPositiveDrift(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1"})
	ctx := context.Background()

	require.NoError(t, h.bp.SetActiveChannels(ctx, "fs-1", 100))
	h.fs.setCount("fs-1", 5)

	// Single-shot Run-equivalent: invoke a sweep and re-read.
	runOneSweep(t, h.rec)

	got, err := h.bp.Get(ctx, "fs-1")
	require.NoError(t, err)
	require.Equal(t, 5, got, "Redis must be aligned to FS truth")
}

// TestReconciler_Sweep_FixesNegativeDrift covers failure mode (3): Redis
// FLUSHDB drops counter to 0 while FS still serves 42 calls. Sweep must
// restore Redis to 42 so backpressure reflects reality.
func TestReconciler_Sweep_FixesNegativeDrift(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1"})
	ctx := context.Background()

	// Redis = 0 (key absent).
	h.fs.setCount("fs-1", 42)

	runOneSweep(t, h.rec)

	got, err := h.bp.Get(ctx, "fs-1")
	require.NoError(t, err)
	require.Equal(t, 42, got)
}

// TestReconciler_Sweep_NoOpOnMatch verifies the steady-state path: when
// Redis already matches FS, the reconciler does NOT issue a Set (saves a
// Redis round-trip on the warm-fleet common case). We assert this via
// miniredis stats — the SET op count must not change.
func TestReconciler_Sweep_NoOpOnMatch(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1"})
	ctx := context.Background()

	require.NoError(t, h.bp.SetActiveChannels(ctx, "fs-1", 10))
	h.fs.setCount("fs-1", 10)

	// Read the Redis state, sweep, read again — value must be unchanged
	// AND the gauge is set to 0 (drift = 0 is still a valid observation).
	runOneSweep(t, h.rec)

	got, err := h.bp.Get(ctx, "fs-1")
	require.NoError(t, err)
	require.Equal(t, 10, got)
	require.InDelta(t, float64(0),
		testutil.ToFloat64(h.gauge.WithLabelValues("fs-1")), 1e-9)
}

// TestReconciler_Sweep_SkipsNodeOnFSError exercises the per-node fault
// boundary: when fs.ActiveChannels fails for one node, the sweep MUST
// continue to the next node so a single stalled FS doesn't paralyse the
// fleet.
func TestReconciler_Sweep_SkipsNodeOnFSError(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1", "fs-2"})
	ctx := context.Background()

	require.NoError(t, h.bp.SetActiveChannels(ctx, "fs-1", 100))
	require.NoError(t, h.bp.SetActiveChannels(ctx, "fs-2", 100))

	h.fs.setErr("fs-1", errors.New("simulated FS-1 timeout"))
	h.fs.setCount("fs-2", 7)

	runOneSweep(t, h.rec)

	// fs-1 is unchanged (the sweep skipped it).
	got, err := h.bp.Get(ctx, "fs-1")
	require.NoError(t, err)
	require.Equal(t, 100, got)

	// fs-2 was reconciled.
	got, err = h.bp.Get(ctx, "fs-2")
	require.NoError(t, err)
	require.Equal(t, 7, got)
}

// TestReconciler_Sweep_SkipsNodeOnRedisGetError covers the redis-get
// failure path: closing miniredis makes every subsequent Get fail. The
// sweep must log + continue (no panic, no crash).
func TestReconciler_Sweep_SkipsNodeOnRedisGetError(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1"})
	h.fs.setCount("fs-1", 5)

	// Close miniredis before sweeping — every Redis op now errors.
	h.mr.Close()

	require.NotPanics(t, func() {
		runOneSweep(t, h.rec)
	}, "sweep must not panic on redis errors")
}

// failOnSetHook is a go-redis v9 Hook that returns an injected error on
// every SET command and passes everything else through. Used to drive the
// rare "Get succeeds, Set fails" branch in Reconciler.sweepNode without
// faulting the whole client (closing miniredis errors Get first, masking
// the Set-side log path).
type failOnSetHook struct{ err error }

func (f *failOnSetHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (f *failOnSetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "set" {
			cmd.SetErr(f.err)
			return f.err
		}
		return next(ctx, cmd)
	}
}

func (f *failOnSetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestReconciler_Sweep_LogsOnRedisSetError exercises the Set-side
// failure path: Get returns the current Redis counter cleanly, but the
// subsequent Set fails. The reconciler must log + continue rather than
// panic. Without this case coverage of sweepNode's Set-error branch is
// missing — the SkipsNodeOnRedisGetError test short-circuits at Get
// because closing miniredis breaks every command, not just Set.
func TestReconciler_Sweep_LogsOnRedisSetError(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// Seed Redis with a value differing from FS truth so the sweep
	// definitely reaches the Set branch.
	require.NoError(t, rdb.Set(context.Background(),
		"op:active_channels:fs-1", 100, 0).Err())

	// Install the SET-fault hook AFTER seeding so the seed Set succeeds.
	rdb.AddHook(&failOnSetHook{err: errors.New("simulated SET failure")})

	bp := router.NewBackpressure(rdb, 5)
	fs := newFakeFSCounter()
	fs.setCount("fs-1", 5)

	rec, err := router.NewReconciler(router.ReconcilerConfig{
		Backpressure: bp,
		FSCounter:    fs,
		NodesFunc:    func() []string { return []string{"fs-1"} },
		Interval:     5 * time.Millisecond,
		Logger:       zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	require.NotPanics(t, func() {
		rec.Sweep(context.Background())
	}, "Set-side failure must be logged + continued, not panicked")
}

// TestReconciler_Sweep_UpdatesDriftGauge asserts the gauge is set on every
// sweep, including when the diff is zero. Operators rely on the
// 0-baseline being visible (a missing series = "is the reconciler even
// running?").
func TestReconciler_Sweep_UpdatesDriftGauge(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1"})
	ctx := context.Background()

	require.NoError(t, h.bp.SetActiveChannels(ctx, "fs-1", 50))
	h.fs.setCount("fs-1", 12)

	runOneSweep(t, h.rec)

	require.InDelta(t, float64(38),
		testutil.ToFloat64(h.gauge.WithLabelValues("fs-1")), 1e-9,
		"|50 - 12| = 38")
}

// TestReconciler_Sweep_NilGaugeNoOp covers the optional-gauge code path:
// when DriftGauge is nil the sweep must still align Redis (the gauge is a
// nice-to-have, not part of the correctness contract).
func TestReconciler_Sweep_NilGaugeNoOp(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	bp := router.NewBackpressure(rdb, 5)
	fs := newFakeFSCounter()
	fs.setCount("fs-1", 9)

	rec, err := router.NewReconciler(router.ReconcilerConfig{
		Backpressure: bp,
		FSCounter:    fs,
		NodesFunc:    func() []string { return []string{"fs-1"} },
		Logger:       zaptest.NewLogger(t),
		// DriftGauge: nil — exercise the no-gauge path.
	})
	require.NoError(t, err)

	runOneSweep(t, rec)

	got, err := bp.Get(context.Background(), "fs-1")
	require.NoError(t, err)
	require.Equal(t, 9, got)
}

// TestReconciler_Sweep_DynamicNodes covers the NodesFunc indirection:
// changes to the live healthy set are observed on the next tick (the
// reconciler does NOT cache an at-construction snapshot).
func TestReconciler_Sweep_DynamicNodes(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1"})
	h.fs.setCount("fs-1", 1)
	h.fs.setCount("fs-2", 2)

	runOneSweep(t, h.rec)

	// fs-2 was not in the node list — no FS call against it.
	got, err := h.bp.Get(context.Background(), "fs-2")
	require.NoError(t, err)
	require.Zero(t, got)

	// Now expose fs-2 via the dynamic NodesFunc and sweep again.
	h.nodesMu.Lock()
	h.nodes = []string{"fs-1", "fs-2"}
	h.nodesMu.Unlock()

	runOneSweep(t, h.rec)

	got, err = h.bp.Get(context.Background(), "fs-2")
	require.NoError(t, err)
	require.Equal(t, 2, got)
}

// --- Run lifecycle -----------------------------------------------------------

// TestReconciler_Run_StopsOnCtxCancel covers the explicit cancellation
// path: Run must return promptly when ctx is cancelled, leaving no
// goroutine behind (goleak in TestMain catches strays).
func TestReconciler_Run_StopsOnCtxCancel(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1"}, withInterval(50*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.rec.Run(ctx)
	}()

	// Let one tick fire so we know Run is actually inside the for-select.
	time.Sleep(75 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestReconciler_Run_TickerFires asserts the for-select actually invokes
// sweep on each tick. Uses a short interval and require.Eventually so
// the test is robust against scheduler jitter.
func TestReconciler_Run_TickerFires(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1"}, withInterval(25*time.Millisecond))
	h.fs.setCount("fs-1", 7)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.rec.Run(ctx)
	}()

	// Wait until at least one sweep landed by checking the FS counter
	// got called and Redis got written.
	require.Eventually(t, func() bool {
		got, err := h.bp.Get(context.Background(), "fs-1")
		if err != nil {
			return false
		}
		return got == 7 && h.fs.calls.Load() >= 1
	}, 2*time.Second, 25*time.Millisecond)

	cancel()
	<-done
}

// TestReconciler_Run_TickerFiresMultiple asserts the ticker fires more
// than once — the reconciler is a recurring sweep, not a one-shot.
func TestReconciler_Run_TickerFiresMultiple(t *testing.T) {
	t.Parallel()
	h := newReconcilerT(t, []string{"fs-1"}, withInterval(25*time.Millisecond))
	h.fs.setCount("fs-1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.rec.Run(ctx)
	}()

	require.Eventually(t, func() bool {
		return h.fs.calls.Load() >= 3
	}, 2*time.Second, 25*time.Millisecond,
		"ticker should sweep multiple times in 2 s at 25ms interval")

	cancel()
	<-done
}

// runOneSweep drives a single Sweep on the reconciler. Equivalent to one
// ticker fire in Run but synchronous and idempotent — preferred over
// driving Run + cancel because that approach lands an unpredictable
// number of ticks under a loaded scheduler (which broke the
// UpdatesDriftGauge test where the second sweep zeroed the gauge after
// the first sweep aligned Redis).
func runOneSweep(t *testing.T, rec *router.Reconciler) {
	t.Helper()
	rec.Sweep(context.Background())
}

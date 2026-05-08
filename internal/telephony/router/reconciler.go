package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// FSCounter reports the live channel count of a FreeSWITCH node, used as
// the source of truth against which the Reconciler aligns the Redis
// op:active_channels counter.
//
// Implementations:
//
//   - ESLFSCounter (this package) issues `api show channels count` over
//     the pool's *esl.Client.
//   - Tests inject a stub via a function field so they can drive
//     deterministic FS-truth values without an ESL handshake.
//
// Errors should preserve esl.ErrNotConnected via %w when the underlying
// FS node is unreachable so the reconciler can log+skip rather than wedge
// on a single failing node.
type FSCounter interface {
	ActiveChannels(ctx context.Context, node string) (int, error)
}

// Compile-time check that ESLFSCounter satisfies the FSCounter contract.
// A signature drift in either type fails compilation here, not at first
// sweep.
var _ FSCounter = (*ESLFSCounter)(nil)

// reconcilerSweepTimeout caps the per-node FS query during a sweep. One
// stalled node MUST NOT block the rest — we'd rather miss a single sweep
// against a slow node than freeze the entire reconciliation cycle.
const reconcilerSweepTimeout = 3 * time.Second

// defaultReconcilerInterval is used when ReconcilerConfig.Interval is
// zero. Plan 09 pegs this at 30 s — long enough that a healthy fleet
// imposes negligible Redis + ESL load, short enough that drift introduced
// by a bridge crash clears in a couple of minutes.
const defaultReconcilerInterval = 30 * time.Second

// Reconciler periodically rewrites the Redis op:active_channels counter
// for every known FS node to match the live channel count reported by
// FreeSWITCH. It is the eventual-consistency safety valve for three
// failure modes:
//
//  1. Bridge crashes after INCR but before the originate command — Redis
//     leaks +1 forever; reconciler drops it back to truth.
//  2. FS node restarts losing all channels — Redis still shows the old
//     count and rejects new originates via backpressure; reconciler
//     resets it to 0 (or whatever FS now reports).
//  3. Redis FLUSHDB or accidental key deletion — Redis shows 0 while
//     FS holds dozens; reconciler restores the real value, otherwise
//     the bridge would over-dispatch and hit the trunk's hard cap.
//
// The Drift gauge is set on every sweep — including when the diff is
// zero — so dashboards reflect a healthy 0-drift baseline. A
// persistently non-zero drift indicates the INCR/DECR path itself has a
// bug the reconciler is masking; that's the signal Plan 09's alert rule
// fires on.
type Reconciler struct {
	bp         *Backpressure
	fs         FSCounter
	nodes      func() []string
	interval   time.Duration
	log        *zap.Logger
	driftGauge *prometheus.GaugeVec
}

// ReconcilerConfig is the Reconciler's input bag. Required fields are
// validated by NewReconciler — the caller learns at boot that wiring is
// missing rather than discovering it three sweeps later from a nil-deref
// panic.
type ReconcilerConfig struct {
	// Backpressure is the handle whose Get + SetActiveChannels methods
	// the reconciler uses to read the current Redis counter and overwrite
	// it with FS truth. Required.
	Backpressure *Backpressure

	// FSCounter reports the FS-truth channel count for a given node.
	// Required.
	FSCounter FSCounter

	// NodesFunc returns the set of nodes to sweep, evaluated on every
	// tick. The production caller passes (*pool.ESLPool).HealthyNodes so
	// the reconciler tracks the live fleet without holding a stale
	// snapshot. Required — a nil function is rejected.
	NodesFunc func() []string

	// Interval is the period between sweeps. Zero falls back to
	// defaultReconcilerInterval (30 s).
	Interval time.Duration

	// Logger is named for the reconciler subsystem; the caller is
	// expected to .Named("reconciler") before passing it in. Nil-tolerated.
	Logger *zap.Logger

	// DriftGauge is the (per-node) gauge to set on every sweep with the
	// absolute diff between Redis and FS. Nil-tolerated; when nil, drift
	// is logged but not exported.
	DriftGauge *prometheus.GaugeVec
}

// NewReconciler validates the wiring and returns a Reconciler ready to
// Run. Validation surfaces missing required deps as a clear error rather
// than as a nil-deref panic at first sweep.
func NewReconciler(cfg ReconcilerConfig) (*Reconciler, error) {
	if cfg.Backpressure == nil {
		return nil, errors.New("router: Reconciler requires Backpressure")
	}
	if cfg.FSCounter == nil {
		return nil, errors.New("router: Reconciler requires FSCounter")
	}
	if cfg.NodesFunc == nil {
		return nil, errors.New("router: Reconciler requires NodesFunc")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultReconcilerInterval
	}
	return &Reconciler{
		bp:         cfg.Backpressure,
		fs:         cfg.FSCounter,
		nodes:      cfg.NodesFunc,
		interval:   interval,
		log:        logger,
		driftGauge: cfg.DriftGauge,
	}, nil
}

// Run blocks until ctx cancels, sweeping every interval. Use as the body
// of an errgroup.Go(...) goroutine in the composition root.
//
// time.NewTicker (not time.After) per references-doc gotcha #1: a ticker
// reuses the same timer across iterations, while time.After in a loop
// allocates one per iteration and leaks each until its deadline expires.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep runs one reconciliation pass over every node returned by
// NodesFunc and returns when every node has been processed (or skipped on
// error). Exposed publicly so tests, an /admin endpoint, or an operator-
// triggered "force a sweep now" rune can drive a single pass without
// waiting for the next ticker fire.
//
// Per-node behaviour:
//
//  1. Fetch FS truth with a 3 s bounded ctx — one stalled node must not
//     wedge the sweep.
//  2. Read the current Redis counter.
//  3. Set Drift gauge to |redis - fs|, including zero (so dashboards see
//     a healthy 0-baseline).
//  4. If they match, no Redis write — saves a round-trip on the steady
//     state, which is the dominant case once the fleet is warm.
//  5. Otherwise rewrite Redis to fs-truth.
//
// Errors at any step log + continue to the next node. The reconciler is
// the safety valve, not a critical path: a missed sweep against one
// stalled node is recovered by the next tick.
func (r *Reconciler) Sweep(ctx context.Context) {
	nodes := r.nodes()
	for _, node := range nodes {
		r.sweepNode(ctx, node)
	}
}

// sweepNode is the per-node body. Pulled out so the for-loop in sweep
// stays linear and so test-side fault injection can target one node
// without iterating through the whole fleet.
//
// Bounded ctx is created and cancelled per-node so the cancel func is
// not deferred inside a for-loop (which would only fire when sweep
// returned, accumulating CancelFunc closures for every node — a slow
// drip leak under high node counts).
func (r *Reconciler) sweepNode(parent context.Context, node string) {
	nctx, cancel := context.WithTimeout(parent, reconcilerSweepTimeout)
	truth, err := r.fs.ActiveChannels(nctx, node)
	cancel()
	if err != nil {
		r.log.Warn("reconciler: fs counter fetch failed",
			zap.String("node", node), zap.Error(err))
		return
	}

	cur, err := r.bp.Get(parent, node)
	if err != nil {
		r.log.Warn("reconciler: redis counter get failed",
			zap.String("node", node), zap.Error(err))
		return
	}

	diff := absDiff(cur, truth)
	if r.driftGauge != nil {
		r.driftGauge.WithLabelValues(node).Set(float64(diff))
	}

	if cur == truth {
		return
	}

	if err := r.bp.SetActiveChannels(parent, node, truth); err != nil {
		r.log.Error("reconciler: redis counter write failed",
			zap.String("node", node), zap.Error(err))
		return
	}

	r.log.Info("reconciler: active_channels reconciled",
		zap.String("node", node),
		zap.Int("redis_was", cur),
		zap.Int("fs_truth", truth),
	)
}

// absDiff returns |a - b| as a non-negative int. Implemented directly
// rather than via math.Abs(float64(a-b)) so we avoid the float round-trip
// for what is purely integer arithmetic — Plan 09 spec uses math.Abs but
// integer subtraction is both faster and free of FP rounding artefacts.
func absDiff(a, b int) int {
	if a >= b {
		return a - b
	}
	return b - a
}

// String is a debug helper used by structured-log fields when the caller
// wants a single-line summary of the reconciler's wiring. Not part of any
// hot path — kept here so a future operator-facing /admin endpoint can
// surface the live config without reaching into private fields.
func (r *Reconciler) String() string {
	return fmt.Sprintf("Reconciler{interval=%s}", r.interval)
}

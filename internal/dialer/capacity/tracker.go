package capacity

import (
	"context"
	"errors"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// Pool is the small surface this package consumes from
// internal/telephony/pool.ESLPool. Declaring it here (rather than
// importing the concrete type) keeps unit tests free of the entire
// telephony stack — production wiring passes *pool.ESLPool, which
// satisfies this via duck typing.
type Pool interface {
	// HealthyNodes returns the addresses of FS nodes the pool currently
	// considers healthy. Empty slice (or nil) means the dialer cannot
	// place any call right now.
	HealthyNodes() []string
}

// Bp is the small surface this package consumes from
// internal/telephony/router.Backpressure. Same duck-typing rationale as
// Pool — the production *router.Backpressure satisfies this verbatim.
type Bp interface {
	// TryAcquire atomically claims one channel slot on node. Returns
	// (true, nil) when the slot was claimed; (false, nil) when the node
	// is at cap; an error wrapping the underlying redis failure on a
	// transport-level fault.
	TryAcquire(ctx context.Context, node string) (bool, error)

	// Release returns one channel slot to node. Idempotent.
	Release(ctx context.Context, node string) error

	// Get returns the current per-node counter (0 on absent key).
	Get(ctx context.Context, node string) (int, error)

	// Cap returns the configured per-node cap.
	Cap() int
}

// Config bundles the dependencies and settings for a Tracker. Required
// fields are documented per-field; nil-tolerated fields fall back to
// safe defaults so the constructor stays trivially wireable from tests.
type Config struct {
	// Pool returns the current healthy-FS-node list. Required.
	// Production passes *pool.ESLPool from internal/telephony/pool;
	// the small interface keeps unit tests light.
	Pool Pool

	// Backpressure is the per-node Redis counter. Required. Production
	// passes *router.Backpressure from internal/telephony/router; the
	// dialer reuses the EXISTING op:active_channels:{node} key family
	// rather than introducing a parallel one.
	Backpressure Bp

	// Logger receives per-method diagnostics. nil → zap.NewNop().
	// Per Plan 09 carry-forward, fields are typed (zap.String /
	// zap.Stringer) and never carry PII — this package only logs node
	// addresses (already non-PII) and error details.
	Logger *zap.Logger

	// Metrics is the per-package collector group. nil → no metrics
	// (the Tracker is fully functional without it).
	Metrics *Metrics
}

// Tracker implements api.LineCapacityTracker by wrapping the existing
// Plan 09 Backpressure Redis counter with a round-robin healthy-node
// selector. Stateless across calls (the round-robin counter is a single
// atomic.Uint64), goroutine-safe without locks.
type Tracker struct {
	pool    Pool
	bp      Bp
	log     *zap.Logger
	metrics *Metrics

	// rrCounter is the round-robin starting offset. Incremented on
	// every Acquire via atomic.Uint64.Add - i.e. the FIRST call's
	// starting index is 0 (counter goes 0 → 1, we use Add - 1 == 0).
	// atomic.Uint64 (not sync.Mutex) per Plan 09 carry-forward — the
	// hot path is a single atomic op, no lock contention under load.
	rrCounter atomic.Uint64
}

// Compile-time interface check. Surfaces api.LineCapacityTracker
// signature drift the moment it happens (per Plan 09 lessons #8).
var _ api.LineCapacityTracker = (*Tracker)(nil)

// New constructs a Tracker. Returns an error when a required dependency
// is missing; nil-tolerated fields are filled with defaults so callers
// can pass a minimal Config{Pool: ..., Backpressure: ...} for the
// simplest wiring.
func New(cfg Config) (*Tracker, error) {
	if cfg.Pool == nil {
		return nil, errors.New("capacity.New: Pool is required")
	}
	if cfg.Backpressure == nil {
		return nil, errors.New("capacity.New: Backpressure is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Tracker{
		pool:    cfg.Pool,
		bp:      cfg.Backpressure,
		log:     logger,
		metrics: cfg.Metrics,
	}, nil
}

// Acquire reserves one channel slot on a healthy FS node and returns
// its address. The walk starts at a round-robin offset (atomic.Uint64
// counter, +1 per call) so even a fully-balanced fleet is exercised
// evenly under sustained load.
//
// Per-node failure modes:
//
//   - TryAcquire returns (true, nil) → success; return that node.
//   - TryAcquire returns (false, nil) → node at cap; try next.
//   - TryAcquire returns (_, err)     → log + skip; try next. We do
//     NOT propagate a single-node transport error to the caller —
//     the dialer's caller wants to know "is there capacity SOMEWHERE",
//     and a flapping single-node Redis path shouldn't paint the whole
//     fleet red.
//
// All nodes exhausted → api.ErrAllNodesFull. Empty healthy list
// → api.ErrAllNodesFull (without any TryAcquire call).
func (t *Tracker) Acquire(ctx context.Context) (string, error) {
	nodes := t.pool.HealthyNodes()
	if len(nodes) == 0 {
		t.metrics.observeAcquire(resultAllFull)
		return "", api.ErrAllNodesFull
	}

	// Round-robin starting offset. Add returns the new value so
	// Add-1 yields the offset for THIS call. uint64 wraps on overflow
	// (~10^19 calls — practically infinite); the modulo by len(nodes)
	// makes wraparound a no-op for correctness.
	start := int(t.rrCounter.Add(1) - 1) //nolint:gosec // modulo wrap is intentional

	for offset := range len(nodes) {
		node := nodes[(start+offset)%len(nodes)]
		ok, err := t.bp.TryAcquire(ctx, node)
		if err != nil {
			// Single-node transport failure: tick error counter,
			// log, and try the next node. The all-nodes-error
			// terminus is api.ErrAllNodesFull — see below.
			t.metrics.observeAcquire(resultError)
			t.log.Warn("backpressure TryAcquire failed; skipping node",
				zap.String("node", node),
				zap.Error(err),
			)
			continue
		}
		if !ok {
			// Node at cap; try next. No metric tick here — the
			// terminal result (ok or all_full) covers the call's
			// success/failure dimension; per-node "at cap" is
			// implicit in dialer_capacity_active{node} == cap.
			continue
		}
		// Success: refresh the per-node active gauge from the
		// authoritative Backpressure counter so dashboards reflect
		// the post-INCR value.
		if v, getErr := t.bp.Get(ctx, node); getErr == nil {
			t.metrics.setActive(node, float64(v))
		}
		t.metrics.observeAcquire(resultOK)
		return node, nil
	}

	// Every node returned ok=false or errored. Surface the sentinel
	// the dialer caller branches on (errors.Is). The metric label
	// "all_full" intentionally covers BOTH "every node at cap" and
	// "every node errored" — operators discriminate via the per-node
	// "error" counter rate (high error rate + all_full → Redis fault;
	// zero error rate + all_full → genuine capacity exhaustion).
	t.metrics.observeAcquire(resultAllFull)
	return "", api.ErrAllNodesFull
}

// Release returns one channel slot to node. Pass-through to
// Backpressure.Release; the caller (dialer worker / FSM hangup path)
// is the keeper of the (call_id → node) mapping and supplies the same
// node string Acquire returned.
//
// Empty node is a wiring bug (caller forgot to pass through the
// Acquire return value); we surface it as an error rather than
// silently no-op so the bug is loud at the call site rather than
// observable only as a slowly-drifting counter on the bridge side.
func (t *Tracker) Release(ctx context.Context, node string) error {
	if node == "" {
		return errors.New("capacity.Release: node must be non-empty")
	}
	if err := t.bp.Release(ctx, node); err != nil {
		t.metrics.observeRelease(resultError)
		t.log.Debug("backpressure Release failed",
			zap.String("node", node),
			zap.Error(err),
		)
		return err
	}
	// Refresh the per-node active gauge from the post-DECR counter.
	if v, getErr := t.bp.Get(ctx, node); getErr == nil {
		t.metrics.setActive(node, float64(v))
	}
	t.metrics.observeRelease(resultOK)
	return nil
}

// Stats returns a per-node snapshot of the current active-channel
// counters. The map is keyed by FS-node address; values are int64 to
// match the api.LineCapacityTracker interface (the Backpressure
// counter is int and fits in int64 trivially).
//
// Per-node failure mode: a Get error on a single node is logged and
// skipped (the node is OMITTED from the result map, NOT zero-valued)
// so a flapping single-node Redis call doesn't blank-out the whole
// stats payload. The function returns nil error in this case — the
// degraded payload is the right answer for dashboards. A genuinely
// catastrophic backend failure surfaces as every-node-error +
// empty-result, which a caller can detect by len(map) == 0 vs. a
// non-empty HealthyNodes slice.
//
// Empty HealthyNodes → empty map (not nil), nil error.
func (t *Tracker) Stats(ctx context.Context) (map[string]int64, error) {
	nodes := t.pool.HealthyNodes()
	out := make(map[string]int64, len(nodes))
	for _, node := range nodes {
		v, err := t.bp.Get(ctx, node)
		if err != nil {
			t.log.Warn("backpressure Get failed; skipping node in Stats",
				zap.String("node", node),
				zap.Error(err),
			)
			continue
		}
		out[node] = int64(v)
		t.metrics.setActive(node, float64(v))
	}
	return out, nil
}

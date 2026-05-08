// Package capacity implements api.LineCapacityTracker — the dialer's
// view of the per-FreeSWITCH-node concurrent-channels cap.
//
// # Why a thin adapter
//
// Plan 09 Task 5 already owns the canonical Redis counter for "channels
// in use on FS node N": op:active_channels:{node}, mutated by a Lua
// INCR-with-cap script in internal/telephony/router/backpressure.go.
// Plan 10 Task 6 deliberately does NOT introduce a parallel counter —
// the dialer reuses that key by wrapping the existing
// router.Backpressure behind the dialer's api.LineCapacityTracker
// interface. A second Redis key would split the source-of-truth between
// the bridge (which decrements on CHANNEL_HANGUP_COMPLETE via
// Plan 09 Task 6's reconciler) and the dialer, and the two counters
// would drift the moment one side missed an event. The single counter
// family op:active_channels:{*} is shared.
//
// # What this package adds on top of router.Backpressure
//
// Two responsibilities the bridge-side Backpressure does not have:
//
//  1. Node selection. The dialer Acquire surface returns A node, not
//     "did node X have room". This package walks Pool.HealthyNodes()
//     using a round-robin starting offset (an atomic.Uint64) so even
//     under steady load no single node is preferentially exhausted.
//
//  2. The api.ErrAllNodesFull sentinel. The dialer caller branches on
//     errors.Is(err, api.ErrAllNodesFull) to distinguish "every healthy
//     FS node is at cap → back off" from a transport-level Redis
//     failure → propagate. This package translates the
//     ok-false-from-every-node case into the sentinel.
//
// Both responsibilities are stateless across calls (the round-robin
// counter is a single atomic.Uint64) so the Tracker is goroutine-safe
// without locks.
//
// # Surface dependencies
//
// The package depends on small interfaces (Pool, Bp) declared in
// tracker.go, NOT on the concrete *pool.ESLPool / *router.Backpressure.
// Production wiring passes those concrete types — they satisfy the
// interfaces via duck typing — but unit tests use lightweight in-memory
// fakes and don't drag the whole telephony surface in.
//
// # Plan 09 carry-forward
//
//   - *zap.Logger with typed fields, never PII.
//   - var _ api.LineCapacityTracker = (*Tracker)(nil) compile-time check.
//   - No init()-time MustRegister; metrics are wired explicitly via
//     RegisterMetrics(reg) so two test imports don't collide on the
//     default registerer.
//   - atomic.Uint64.Add for the round-robin counter, not sync.Mutex —
//     the round-robin step is a single hot atomic op per Acquire.
package capacity

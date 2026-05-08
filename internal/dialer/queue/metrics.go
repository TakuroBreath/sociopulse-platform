package queue

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by the call queue.
// The package follows the same nil-tolerant pattern as the FSM metrics:
// every observe* helper checks for a nil receiver, so a Machine /
// RedisQueue constructed without metrics still works. The composition
// root passes a *Metrics into Config.Metrics; tests pass either nil
// (simple unit tests) or a freshly-built one against
// prometheus.NewRegistry() (assertion tests).
//
// Like Plan 09's per-package metrics, this package deliberately does NOT
// register collectors at init() time. Two test imports of an init()-
// registering package collide on prometheus.DefaultRegisterer; instead
// the composition root passes a scoped *prometheus.Registry into
// RegisterMetrics, and each test that exercises metrics builds its own.
type Metrics struct {
	// Enqueue counts every EnqueueRespondent attempt, partitioned by
	// outcome ("ok", "duplicate", "error"). High-cardinality is bounded
	// (3 results × 1 metric series).
	Enqueue *prometheus.CounterVec

	// Pickup counts every PickNext attempt, partitioned by outcome
	// ("ok", "empty", "error"). The "empty" bucket is the steady-state
	// signal that the worker pool is over-provisioned for the current
	// queue depth.
	Pickup *prometheus.CounterVec

	// Requeue counts every Requeue invocation. Single-series — the
	// requeue path does not branch on outcome at the metric layer
	// (errors surface via the Pickup error path indirectly when the
	// requeue fails on Redis transport).
	Requeue prometheus.Counter

	// Size is the per-tenant per-project queue depth. Set on every
	// Size() call. The {tenant, project} cardinality is bounded by the
	// number of active projects, which is a small N in production
	// (one tenant typically has ≤ 50 projects).
	Size *prometheus.GaugeVec
}

// RegisterMetrics builds a fresh *Metrics and registers every collector
// on the supplied registerer. The caller owns the registerer's lifetime
// — production wires `metrics.Registry` from `pkg/observability`; tests
// pass `prometheus.NewRegistry()`.
//
// reg must be non-nil; a nil registerer is a wiring error and panics
// here so the failure surfaces at boot instead of at first metric
// emission. Panics also on duplicate registration for the same reason.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("queue.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		Enqueue: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_queue_enqueue_total",
				Help: "Total CallQueue.EnqueueRespondent invocations, by result (ok|duplicate|error).",
			},
			[]string{"result"},
		),
		Pickup: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_queue_pickup_total",
				Help: "Total CallQueue.PickNext invocations, by result (ok|empty|error).",
			},
			[]string{"result"},
		),
		Requeue: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "dialer_queue_requeue_total",
				Help: "Total CallQueue.Requeue invocations.",
			},
		),
		Size: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dialer_queue_size",
				Help: "Current CallQueue depth by tenant and project, set on every Size() call.",
			},
			[]string{"tenant", "project"},
		),
	}
	reg.MustRegister(m.Enqueue, m.Pickup, m.Requeue, m.Size)
	return m
}

// Result label constants. Defined as named values so the call sites stay
// typo-resistant and the cardinality bound is enforced at compile time.
const (
	resultOK        = "ok"
	resultDuplicate = "duplicate"
	resultEmpty     = "empty"
	resultError     = "error"
)

// observeEnqueue increments the Enqueue counter for the given result.
// nil-tolerated so the queue works without metrics in tests.
func (m *Metrics) observeEnqueue(result string) {
	if m == nil || m.Enqueue == nil {
		return
	}
	m.Enqueue.WithLabelValues(result).Inc()
}

// observePickup increments the Pickup counter for the given result.
// nil-tolerated.
func (m *Metrics) observePickup(result string) {
	if m == nil || m.Pickup == nil {
		return
	}
	m.Pickup.WithLabelValues(result).Inc()
}

// observeRequeue increments the Requeue counter. nil-tolerated.
func (m *Metrics) observeRequeue() {
	if m == nil || m.Requeue == nil {
		return
	}
	m.Requeue.Inc()
}

// observeSize sets the Size gauge for the given tenant/project pair.
// nil-tolerated.
func (m *Metrics) observeSize(tenantID, projectID string, size int64) {
	if m == nil || m.Size == nil {
		return
	}
	m.Size.WithLabelValues(tenantID, projectID).Set(float64(size))
}

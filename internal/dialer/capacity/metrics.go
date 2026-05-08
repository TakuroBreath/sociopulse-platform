package capacity

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by the dialer
// LineCapacityTracker. Per Plan 09 lessons, this package deliberately
// does NOT register collectors at init() time — two test imports of an
// init()-registering package collide on prometheus.DefaultRegisterer.
// Instead the composition root passes a scoped *prometheus.Registry into
// RegisterMetrics, and each test that exercises metrics builds its own
// fresh registry.
//
// All observe* helpers tolerate a nil receiver so the Tracker works
// without metrics in unit tests.
type Metrics struct {
	// Acquires counts every Acquire invocation, partitioned by result.
	// Bounded cardinality (3 series): "ok", "all_full", "error".
	Acquires *prometheus.CounterVec

	// Releases counts every Release invocation, partitioned by result.
	// Bounded cardinality (2 series): "ok", "error".
	Releases *prometheus.CounterVec

	// Active is a per-node gauge of the current channel count, populated
	// by Stats() callers and by the Tracker on every successful Acquire/
	// Release. Cardinality is bounded by the configured FS node count
	// (~2-4 in production).
	Active *prometheus.GaugeVec
}

// RegisterMetrics builds a fresh *Metrics and registers every collector
// on the supplied registerer. The caller owns the registerer's lifetime
// — production wires metrics.Registry from pkg/observability; tests
// pass prometheus.NewRegistry().
//
// reg must be non-nil; a nil registerer is a wiring error and panics
// here so the failure surfaces at boot instead of at first metric
// emission. Panics also on duplicate registration for the same reason.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("capacity.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		Acquires: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_capacity_acquire_total",
				Help: "Total LineCapacityTracker.Acquire invocations, by result (ok|all_full|error).",
			},
			[]string{"result"},
		),
		Releases: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_capacity_release_total",
				Help: "Total LineCapacityTracker.Release invocations, by result (ok|error).",
			},
			[]string{"result"},
		),
		Active: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dialer_capacity_active",
				Help: "Current per-node active-channel count as observed by the dialer LineCapacityTracker.",
			},
			[]string{"node"},
		),
	}
	reg.MustRegister(
		m.Acquires,
		m.Releases,
		m.Active,
	)
	return m
}

// Result label constants. Defined as named values so the call sites
// stay typo-resistant and the cardinality bound is enforced at compile
// time.
const (
	resultOK      = "ok"
	resultAllFull = "all_full"
	resultError   = "error"
)

// observeAcquire increments the Acquires counter for the given result.
// nil-tolerated.
func (m *Metrics) observeAcquire(result string) {
	if m == nil || m.Acquires == nil {
		return
	}
	m.Acquires.WithLabelValues(result).Inc()
}

// observeRelease increments the Releases counter for the given result.
// nil-tolerated.
func (m *Metrics) observeRelease(result string) {
	if m == nil || m.Releases == nil {
		return
	}
	m.Releases.WithLabelValues(result).Inc()
}

// setActive sets the per-node Active gauge to v. nil-tolerated.
func (m *Metrics) setActive(node string, v float64) {
	if m == nil || m.Active == nil {
		return
	}
	m.Active.WithLabelValues(node).Set(v)
}

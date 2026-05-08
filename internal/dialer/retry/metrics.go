package retry

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by the dialer retry
// orchestrator. Per Plan 09 lessons, this package deliberately does NOT
// register collectors at init() time — two test imports of an init()-
// registering package collide on prometheus.DefaultRegisterer. Instead
// the composition root passes a scoped *prometheus.Registry into
// RegisterMetrics, and each test that exercises metrics builds its own
// fresh registry.
//
// All observe* helpers tolerate a nil receiver so the orchestrator
// works without metrics in unit tests.
type Metrics struct {
	// Sweeps counts every Apply / re-enqueue decision the orchestrator
	// makes during a sweep. Bounded cardinality (4 series):
	// "enqueued" | "exhausted" | "dnc" | "skip".
	Sweeps *prometheus.CounterVec

	// LeaderActive is 1 on the instance currently holding the advisory
	// lock and 0 elsewhere. Exactly one replica should report 1 at a
	// time across the cluster.
	LeaderActive prometheus.Gauge

	// SweepDuration is a histogram of full sweep durations (PG read +
	// per-row decisions). Buckets cover the expected operating range
	// (10ms .. 30s).
	SweepDuration prometheus.Histogram
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
		panic("retry.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		Sweeps: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_retry_due_total",
				Help: "Total retry-orchestrator outcomes per row, by result (enqueued|exhausted|dnc|skip).",
			},
			[]string{"result"},
		),
		LeaderActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dialer_retry_leader_active",
			Help: "1 when this instance holds the retry-orchestrator advisory lock; 0 otherwise.",
		}),
		SweepDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "dialer_retry_sweep_duration_seconds",
			Help:    "Time taken by one retry-orchestrator sweep (PG read + per-row decisions + queue + DB updates).",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms .. ~40s
		}),
	}
	reg.MustRegister(
		m.Sweeps,
		m.LeaderActive,
		m.SweepDuration,
	)
	return m
}

// Result label constants for the Sweeps counter. Named values keep the
// call-site typo-resistant and the cardinality bound explicit.
const (
	resultEnqueued  = "enqueued"
	resultExhausted = "exhausted"
	resultDNC       = "dnc"
	resultSkip      = "skip"
)

// observeSweep increments the per-row outcome counter. nil-tolerated.
func (m *Metrics) observeSweep(result string) {
	if m == nil || m.Sweeps == nil {
		return
	}
	m.Sweeps.WithLabelValues(result).Inc()
}

// setLeaderActive writes the leader gauge. nil-tolerated. Pass true on
// successful Acquire and false after Release / failed Acquire so peers'
// dashboards stay consistent.
func (m *Metrics) setLeaderActive(active bool) {
	if m == nil || m.LeaderActive == nil {
		return
	}
	v := 0.0
	if active {
		v = 1.0
	}
	m.LeaderActive.Set(v)
}

// observeSweepDuration records one sweep's wall-clock duration. nil-
// tolerated.
func (m *Metrics) observeSweepDuration(seconds float64) {
	if m == nil || m.SweepDuration == nil {
		return
	}
	m.SweepDuration.Observe(seconds)
}

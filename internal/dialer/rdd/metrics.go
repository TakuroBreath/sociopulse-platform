package rdd

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by the RDD generator.
// Like Plan 09's per-package metrics, we register through an explicit
// [RegisterMetrics] call (NOT init()) so two test imports do not collide
// on the default registerer. The composition root passes the production
// registry; tests pass prometheus.NewRegistry().
type Metrics struct {
	// Generated counts every Generate iteration's outcome, partitioned by
	// the result label. Bounded cardinality: 5 outcomes total (ok |
	// duplicate | dnc | invalid | throttled).
	Generated *prometheus.CounterVec

	// Duration is a histogram of end-to-end Generate latency. Bucketed
	// for the typical operator-trigger range (10ms – 5s); higher
	// observations clip into the +Inf bucket and surface as p99 spikes
	// in the dashboard.
	Duration prometheus.Histogram
}

// Result label constants — typed strings prevent typos at the call site
// and bound the cardinality of [Metrics.Generated].
const (
	resultOK        = "ok"
	resultDuplicate = "duplicate"
	resultDNC       = "dnc"
	resultInvalid   = "invalid"
	resultThrottled = "throttled"
)

// RegisterMetrics builds a fresh *Metrics and registers every collector
// on the supplied registerer. Production wires `metrics.Registry` from
// `pkg/observability`; tests pass `prometheus.NewRegistry()`.
//
// reg must be non-nil; a nil registerer is a wiring error and panics
// here so the failure surfaces at boot rather than at the first metric
// emission. Panics on duplicate registration for the same reason.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("rdd.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		Generated: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_rdd_generated_total",
				Help: "Total RDD Generate iterations, by result (ok|duplicate|dnc|invalid|throttled).",
			},
			[]string{"result"},
		),
		Duration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "dialer_rdd_generate_duration_seconds",
				Help:    "End-to-end RDD Generate latency in seconds.",
				Buckets: prometheus.ExponentialBuckets(0.01, 2.0, 10), // 10ms → ~5.12s
			},
		),
	}
	reg.MustRegister(m.Generated, m.Duration)
	return m
}

// observe increments the Generated counter for the given result.
// nil-tolerated so the generator works without metrics in tests.
func (m *Metrics) observe(result string) {
	if m == nil || m.Generated == nil {
		return
	}
	m.Generated.WithLabelValues(result).Inc()
}

// observeDuration records the end-to-end Generate latency in seconds.
// nil-tolerated.
func (m *Metrics) observeDuration(seconds float64) {
	if m == nil || m.Duration == nil {
		return
	}
	m.Duration.Observe(seconds)
}

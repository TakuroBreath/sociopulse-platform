package pool

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by this package. The
// fields stay exported so test code can poke them; production callers
// receive a *Metrics from RegisterMetrics and pass it into Config.Metrics.
//
// Like esl.Metrics, the package deliberately does NOT register collectors
// at init() time. Two test imports of a single-init() package collide on
// prometheus.DefaultRegisterer; instead the composition root passes a
// scoped *prometheus.Registerer into RegisterMetrics, and each test that
// exercises metrics builds its own fresh registry.
type Metrics struct {
	// NodeHealthy is 1 when the labelled FS node has passed its initial
	// health-gate and continues to satisfy the periodic probe; 0
	// otherwise. Gauge per node — operators graph this to see fleet
	// health at a glance.
	NodeHealthy *prometheus.GaugeVec

	// HealthCheckDur observes the wall-clock latency of every SofiaStatus
	// probe (initial gate + periodic). Histogram per node — a sustained
	// p99 climb is the canary for FS-side congestion before it becomes
	// an outright probe failure.
	HealthCheckDur *prometheus.HistogramVec

	// EventsForwarded counts every event the pool fans out, partitioned
	// by node and event-name. The synthetic event="_dropped" label
	// records backpressure-driven drops so on-call can correlate
	// downstream consumer lag with lost events.
	EventsForwarded *prometheus.CounterVec

	// Reconnects counts connect attempts the supervisor makes,
	// partitioned by node and outcome ("ok" / "err"). The "err" series
	// is the canary for a flapping FS node; "ok" rate is a useful
	// fleet-wide reconnect-budget signal.
	Reconnects *prometheus.CounterVec
}

// RegisterMetrics builds a fresh *Metrics and registers every collector
// on the supplied registerer. The caller owns the registerer's lifetime
// — a typical wiring is `metrics.Registry` from pkg/observability, but
// tests pass `prometheus.NewRegistry()`.
//
// reg must be non-nil; a nil registerer is a wiring error and panics
// here so the failure surfaces at boot instead of at first metric
// emission. Panics also on duplicate registration for the same reason.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("pool.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		NodeHealthy: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "telephony_pool_node_healthy",
				Help: "1 if the FS node is currently healthy in the pool, 0 otherwise.",
			},
			[]string{"node"},
		),
		HealthCheckDur: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "telephony_pool_health_check_seconds",
				Help:    "Wall-clock latency of pool SofiaStatus health probes, by node.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"node"},
		),
		EventsForwarded: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telephony_pool_events_forwarded_total",
				Help: "Total events fanned out by the pool, by node and event name. event=\"_dropped\" counts backpressure drops.",
			},
			[]string{"node", "event"},
		),
		Reconnects: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telephony_pool_reconnects_total",
				Help: "Total connect attempts the pool supervisor has made, by node and outcome.",
			},
			[]string{"node", "result"},
		),
	}
	reg.MustRegister(
		m.NodeHealthy,
		m.HealthCheckDur,
		m.EventsForwarded,
		m.Reconnects,
	)
	return m
}

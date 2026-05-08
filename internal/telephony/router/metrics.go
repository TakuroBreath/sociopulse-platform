package router

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by the router. Mirrors the
// pattern in internal/telephony/pool/metrics.go: collectors are NEVER
// registered at init() — two test imports of a single-init() package collide
// on the default registerer. The composition root passes a scoped
// *prometheus.Registerer into RegisterMetrics; tests build a fresh
// prometheus.NewRegistry() per case.
type Metrics struct {
	// SelectsTotal counts every Router.Select call partitioned by the
	// strategy name and the outcome. result label values:
	//   - "ok"         — a {trunk, node} pair was returned
	//   - "no_trunk"   — strategy returned ErrNoTrunkAvailable
	//   - "no_node"    — strategy picked a trunk but no healthy FS node
	//                    matched and accepted the backpressure INCR
	//   - "err"        — backpressure or strategy returned a wrapped redis
	//                    failure
	SelectsTotal *prometheus.CounterVec

	// SelectDuration observes wall-clock latency of Router.Select. Useful
	// to graph the p99 hit by the dialer when load spikes; the histogram
	// uses prometheus.DefBuckets which covers the expected single-digit
	// millisecond range with enough resolution above 100 ms to surface
	// Redis round-trip stalls.
	SelectDuration *prometheus.HistogramVec

	// BackpressureRejects counts TryAcquire calls that returned ok=false
	// (i.e. the cap was hit). Partitioned by node so on-call can see which
	// FS node is the bottleneck.
	BackpressureRejects *prometheus.CounterVec

	// Drift is the absolute difference between the Redis op:active_channels
	// counter and the FS-truth value reported by `api show channels count`,
	// per node. Set on every Reconciler sweep — including when the diff is
	// zero — so operators can graph drift-over-time and alert on
	// max(drift) > N for 5m. A non-zero value means the reconciler is
	// actively correcting; a persistently-non-zero value means the
	// underlying INCR/DECR path has a bug that the reconciler is masking.
	Drift *prometheus.GaugeVec
}

// RegisterMetrics builds a fresh *Metrics and registers every collector on
// the supplied registerer. reg must be non-nil — the panic at boot time is
// preferable to a silent miss-registration that would surface only as
// missing /metrics series weeks later.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("router.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		SelectsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telephony_router_selects_total",
				Help: "Total Router.Select calls by strategy and result (ok, no_trunk, no_node, err).",
			},
			[]string{"strategy", "result"},
		),
		SelectDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "telephony_router_select_seconds",
				Help:    "Wall-clock latency of Router.Select, by strategy.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"strategy"},
		),
		BackpressureRejects: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telephony_router_backpressure_rejects_total",
				Help: "Total TryAcquire calls rejected because the per-node cap was reached, by node.",
			},
			[]string{"node"},
		),
		Drift: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "telephony_router_active_channels_drift",
				Help: "Absolute difference between Redis op:active_channels counter and FS truth, by node. Set on every Reconciler sweep (Plan 09 Task 6).",
			},
			[]string{"node"},
		),
	}
	reg.MustRegister(
		m.SelectsTotal,
		m.SelectDuration,
		m.BackpressureRejects,
		m.Drift,
	)
	return m
}

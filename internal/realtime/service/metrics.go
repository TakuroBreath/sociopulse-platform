package service

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics groups the Prometheus collectors emitted by the realtime
// service layer. Construction is gated behind RegisterMetrics so the
// composition root can attach a scoped *prometheus.Registry — no
// init()-time MustRegister, matching the Plan 09/10 carry-forward
// rule.
type Metrics struct {
	// DroppedFrames counts frames dropped by Connection.Send because
	// the per-connection sendChan was full. The {conn_id} label is
	// bounded by the lifetime of the connection (one connection
	// disappears -> Prometheus eventually GCs the series).
	DroppedFrames *prometheus.CounterVec

	// AuthFailures counts auth-handshake failures, partitioned by
	// reason ("read_error", "bad_json", "wrong_kind", "missing_token",
	// "invalid_token"). Bounded label set; safe for high cardinality
	// dashboards.
	AuthFailures *prometheus.CounterVec

	// PongMisses counts connections closed because the pong-grace
	// window expired without a fresh inbound pong. A non-zero rate
	// indicates a flaky operator network or proxy-level idle dropouts.
	PongMisses prometheus.Counter

	// RateLimitClosures counts connections closed for exceeding the
	// inbound frame-rate budget. Distinct from PongMisses so
	// dashboards can separate "client was misbehaving" from "client
	// was disconnected".
	RateLimitClosures prometheus.Counter
}

// RegisterMetrics builds a fresh *Metrics and registers every
// collector on the supplied registerer. The caller owns the
// registerer's lifetime — production wiring uses
// pkg/observability.Metrics.Registry; tests pass
// prometheus.NewRegistry().
//
// reg must be non-nil; a nil registerer is a wiring error and panics
// here so failure surfaces at boot, not at first metric emission.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("service.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		DroppedFrames: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_dropped_frames_total",
				Help: "Total realtime frames dropped due to slow consumer (sendChan full).",
			},
			[]string{"conn_id"},
		),
		AuthFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_auth_failures_total",
				Help: "Total realtime auth-handshake failures, by reason.",
			},
			[]string{"reason"},
		),
		PongMisses: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "realtime_pong_misses_total",
				Help: "Total connections closed because pong-grace expired.",
			},
		),
		RateLimitClosures: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "realtime_rate_limit_closures_total",
				Help: "Total connections closed for exceeding inbound frame-rate budget.",
			},
		),
	}
	reg.MustRegister(
		m.DroppedFrames,
		m.AuthFailures,
		m.PongMisses,
		m.RateLimitClosures,
	)
	return m
}

// observeDrop increments DroppedFrames for the given connection.
// nil-tolerated so tests without metrics keep working.
func (m *Metrics) observeDrop(connID string) {
	if m == nil || m.DroppedFrames == nil {
		return
	}
	m.DroppedFrames.WithLabelValues(connID).Inc()
}

// observeAuthFailure increments AuthFailures for the given reason.
// nil-tolerated.
func (m *Metrics) observeAuthFailure(reason string) {
	if m == nil || m.AuthFailures == nil {
		return
	}
	m.AuthFailures.WithLabelValues(reason).Inc()
}

// observePongMiss increments PongMisses. nil-tolerated.
func (m *Metrics) observePongMiss() {
	if m == nil || m.PongMisses == nil {
		return
	}
	m.PongMisses.Inc()
}

// observeRateLimitClosure increments RateLimitClosures. nil-tolerated.
func (m *Metrics) observeRateLimitClosure() {
	if m == nil || m.RateLimitClosures == nil {
		return
	}
	m.RateLimitClosures.Inc()
}

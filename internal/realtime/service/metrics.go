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

// HubMetrics groups the Prometheus collectors emitted by the Hub. The
// Hub-level counters live on a separate struct from the
// per-connection Metrics so the Hub can be tested in isolation
// (without a *Connection) and so the composition root can attach a
// scoped registerer per layer.
//
// Construction is gated behind RegisterHubMetrics — same boot
// discipline as RegisterMetrics. Plan 09/10 carry-forward (no
// init()-time MustRegister).
type HubMetrics struct {
	// Connections is a gauge of currently-registered connections.
	// Maintained by Hub.Connect (+1) / disconnect callback (-1).
	Connections prometheus.Gauge

	// Subscriptions is a gauge of active subscriptions, partitioned
	// by topic. Bounded label set (six topics) — safe for dashboards.
	Subscriptions *prometheus.GaugeVec

	// BroadcastsTotal counts Hub.Broadcast invocations by topic.
	// Useful for spotting a runaway publisher (one tenant flooding
	// TopicCallEvents). Pairs with BroadcastFanout for ratio analysis.
	BroadcastsTotal *prometheus.CounterVec

	// BroadcastFanout counts the cumulative number of conn.Send
	// dispatches issued by Broadcast, partitioned by topic. Divided
	// by BroadcastsTotal it gives the average fan-out per topic —
	// dashboards alert on ratio drops (subscriber leakage) or spikes
	// (subscription explosion).
	BroadcastFanout *prometheus.CounterVec

	// SubscribeFailures counts Subscribe RBAC rejections, partitioned
	// by topic + reason ("forbidden", "filter_required", "unknown").
	// Bounded labels.
	SubscribeFailures *prometheus.CounterVec
}

// RegisterHubMetrics builds a fresh *HubMetrics and registers every
// collector on the supplied registerer. nil registerer panics — same
// rule as RegisterMetrics.
func RegisterHubMetrics(reg prometheus.Registerer) *HubMetrics {
	if reg == nil {
		panic("service.RegisterHubMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &HubMetrics{
		Connections: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "realtime_hub_connections",
				Help: "Current number of WebSocket connections registered with the realtime Hub.",
			},
		),
		Subscriptions: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "realtime_hub_subscriptions",
				Help: "Current number of active subscriptions, by topic.",
			},
			[]string{"topic"},
		),
		BroadcastsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_hub_broadcasts_total",
				Help: "Total Hub.Broadcast invocations, by topic.",
			},
			[]string{"topic"},
		),
		BroadcastFanout: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_hub_broadcast_fanout_total",
				Help: "Total conn.Send dispatches issued by Hub.Broadcast, by topic.",
			},
			[]string{"topic"},
		),
		SubscribeFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_hub_subscribe_failures_total",
				Help: "Total Subscribe rejections at the Hub, by topic and reason.",
			},
			[]string{"topic", "reason"},
		),
	}
	reg.MustRegister(
		m.Connections,
		m.Subscriptions,
		m.BroadcastsTotal,
		m.BroadcastFanout,
		m.SubscribeFailures,
	)
	return m
}

// observeConnect increments the Connections gauge. nil-tolerated.
func (m *HubMetrics) observeConnect() {
	if m == nil || m.Connections == nil {
		return
	}
	m.Connections.Inc()
}

// observeDisconnect decrements the Connections gauge. nil-tolerated.
func (m *HubMetrics) observeDisconnect() {
	if m == nil || m.Connections == nil {
		return
	}
	m.Connections.Dec()
}

// observeSubscribe increments the per-topic Subscriptions gauge.
// nil-tolerated.
func (m *HubMetrics) observeSubscribe(topic string) {
	if m == nil || m.Subscriptions == nil {
		return
	}
	m.Subscriptions.WithLabelValues(topic).Inc()
}

// observeUnsubscribe decrements the per-topic Subscriptions gauge.
// nil-tolerated.
func (m *HubMetrics) observeUnsubscribe(topic string) {
	if m == nil || m.Subscriptions == nil {
		return
	}
	m.Subscriptions.WithLabelValues(topic).Dec()
}

// observeBroadcast records a Hub.Broadcast invocation and the
// resulting fan-out count. nil-tolerated.
func (m *HubMetrics) observeBroadcast(topic string, fanout int) {
	if m == nil {
		return
	}
	if m.BroadcastsTotal != nil {
		m.BroadcastsTotal.WithLabelValues(topic).Inc()
	}
	if m.BroadcastFanout != nil && fanout > 0 {
		m.BroadcastFanout.WithLabelValues(topic).Add(float64(fanout))
	}
}

// observeSubscribeFailure records a Subscribe rejection by the RBAC
// matrix or the unknown-topic guard. nil-tolerated.
func (m *HubMetrics) observeSubscribeFailure(topic, reason string) {
	if m == nil || m.SubscribeFailures == nil {
		return
	}
	m.SubscribeFailures.WithLabelValues(topic, reason).Inc()
}

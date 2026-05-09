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
	// the per-connection telemetryCh was full (drop-oldest path).
	// The {conn_id} label is bounded by the lifetime of the
	// connection (one connection disappears -> Prometheus eventually
	// GCs the series).
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

	// criticalOverflows counts connections closed due to
	// criticalCh overflow (Plan 11.2 dual-queue routing). The
	// {conn_id} label disappears from Prometheus once the connection
	// goes away — bounded cardinality.
	criticalOverflows *prometheus.CounterVec

	// unknownTopicClasses counts Connection.Send invocations with a
	// topic missing from rtapi.TopicClass. {topic} label is bounded
	// because the wiring path validates against AllTopics — an
	// unbounded payload-string-as-topic surfaces as a closed
	// connection (CloseProtocolErr), not unbounded series.
	unknownTopicClasses *prometheus.CounterVec
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
				Help: "Total realtime frames dropped due to slow consumer (telemetryCh full, drop-oldest).",
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
		criticalOverflows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "realtime_critical_overflows_total",
			Help: "Number of WS connections closed due to critical-queue overflow.",
		}, []string{"conn_id"}),
		unknownTopicClasses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "realtime_unknown_topic_classes_total",
			Help: "Number of Send() calls with a topic missing FrameClass mapping (wiring bug indicator).",
		}, []string{"topic"}),
	}
	reg.MustRegister(
		m.DroppedFrames,
		m.AuthFailures,
		m.PongMisses,
		m.RateLimitClosures,
		m.criticalOverflows,
		m.unknownTopicClasses,
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

// observeCriticalOverflow ticks when a critical frame can't fit on
// criticalCh and the connection is closed as a result. nil-safe.
func (m *Metrics) observeCriticalOverflow(connID string) {
	if m == nil || m.criticalOverflows == nil {
		return
	}
	m.criticalOverflows.WithLabelValues(connID).Inc()
}

// observeUnknownTopicClass ticks when Connection.Send is called with
// a topic that has no FrameClass mapping. Cardinality bounded by the
// `topic` label which is checked against AllTopics in the wiring
// path; an unbounded payload-string-as-topic would surface as the
// connection being closed with CloseProtocolErr — see Send.
func (m *Metrics) observeUnknownTopicClass(topic string) {
	if m == nil || m.unknownTopicClasses == nil {
		return
	}
	m.unknownTopicClasses.WithLabelValues(topic).Inc()
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

// presenceTouchResult* are the bounded label values emitted by
// PresenceMetrics.Touch. Stringly-typed constants rather than enum
// values so the Prometheus output stays stable across refactors.
const (
	presenceTouchResultOK     = "ok"
	presenceTouchResultLapsed = "lapsed"
	presenceTouchResultError  = "error"
)

// PresenceMetrics groups the Prometheus collectors emitted by
// RedisPresenceTracker. Same construction discipline as Metrics /
// HubMetrics — RegisterPresenceMetrics gates the registerer.
//
// The four counters cover the full presence lifecycle:
//   - Connect / Disconnect for raw event volume.
//   - Touch{result} for liveness ratio dashboards (a rising
//     `lapsed` rate flags a stuck Hub touch loop).
//   - OnlineUsers (gauge) for per-tenant operator concurrency. The
//     tenant_id label is bounded by the project's small tenant set
//     (~30 in production) — safe.
type PresenceMetrics struct {
	// Connect counts successful OnConnect calls. No labels — every
	// connect is a single integer increment.
	Connect prometheus.Counter

	// Disconnect counts successful OnDisconnect calls. No labels.
	Disconnect prometheus.Counter

	// Touch counts Touch invocations partitioned by outcome:
	// {ok, lapsed, error}. Dashboards alert on lapsed/total ratio.
	Touch *prometheus.CounterVec

	// OnlineUsers is a gauge of currently-online users per tenant.
	// The tracker does NOT auto-update this gauge — the value is set
	// explicitly by the periodic snapshotter (Plan 11 Task 10's
	// janitor) to avoid double-counting when multiple replicas hit
	// OnConnect for the same user. Unlabelled construction would
	// hide the per-tenant view, so we accept the label even though
	// the tracker itself doesn't write it.
	OnlineUsers *prometheus.GaugeVec
}

// RegisterPresenceMetrics builds a *PresenceMetrics and registers
// every collector on reg. nil registerer panics — same boot rule as
// RegisterMetrics / RegisterHubMetrics.
func RegisterPresenceMetrics(reg prometheus.Registerer) *PresenceMetrics {
	if reg == nil {
		panic("service.RegisterPresenceMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &PresenceMetrics{
		Connect: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "realtime_presence_connect_total",
				Help: "Total OnConnect events recorded by the realtime presence tracker.",
			},
		),
		Disconnect: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "realtime_presence_disconnect_total",
				Help: "Total OnDisconnect events recorded by the realtime presence tracker.",
			},
		),
		Touch: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_presence_touch_total",
				Help: "Total Touch invocations on the realtime presence tracker, by result (ok|lapsed|error).",
			},
			[]string{"result"},
		),
		OnlineUsers: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "realtime_presence_online_users_count",
				Help: "Current number of online users per tenant.",
			},
			[]string{"tenant_id"},
		),
	}
	reg.MustRegister(m.Connect, m.Disconnect, m.Touch, m.OnlineUsers)
	return m
}

// observePresenceConnect increments the Connect counter.
// nil-tolerated so a tracker without metrics keeps working.
func (m *PresenceMetrics) observePresenceConnect() {
	if m == nil || m.Connect == nil {
		return
	}
	m.Connect.Inc()
}

// observePresenceDisconnect increments the Disconnect counter.
// nil-tolerated.
func (m *PresenceMetrics) observePresenceDisconnect() {
	if m == nil || m.Disconnect == nil {
		return
	}
	m.Disconnect.Inc()
}

// observePresenceTouch increments the Touch counter for the given
// result. Result values are constrained to the package-private
// presenceTouchResult* constants.
func (m *PresenceMetrics) observePresenceTouch(result string) {
	if m == nil || m.Touch == nil {
		return
	}
	m.Touch.WithLabelValues(result).Inc()
}

// SetOnlineUsers updates the OnlineUsers gauge for the given tenant.
// Exposed publicly so Plan 11 Task 10's janitor (in a separate file
// in this package) can publish authoritative counts without
// double-counting via per-OnConnect updates. nil-tolerated.
func (m *PresenceMetrics) SetOnlineUsers(tenantID string, count int) {
	if m == nil || m.OnlineUsers == nil {
		return
	}
	m.OnlineUsers.WithLabelValues(tenantID).Set(float64(count))
}

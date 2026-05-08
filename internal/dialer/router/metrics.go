package router

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by the dialer Router.
// Like Plan 09's per-package metrics, this package deliberately does NOT
// register collectors at init() time. Two test imports of an init()-
// registering package collide on prometheus.DefaultRegisterer; instead
// the composition root passes a scoped *prometheus.Registry into
// RegisterMetrics, and each test that exercises metrics builds its own.
//
// All observe* helpers tolerate a nil receiver so the Router works
// without metrics in unit tests.
type Metrics struct {
	// Dials counts every Dial invocation, partitioned by outcome
	// ("ok", "error"). Bounded cardinality (2 series).
	Dials *prometheus.CounterVec

	// Hangups counts every Hangup invocation, partitioned by outcome
	// ("ok", "error"). Bounded cardinality (2 series).
	Hangups *prometheus.CounterVec

	// EventsReceived counts every ChannelEvent the Router observes
	// from the underlying telephony.EventConsumer, partitioned by the
	// raw telephony.ChannelEventType label. Cardinality is bounded by
	// the 7 event types declared in telephony.api (dialing, answer,
	// bridge, unbridge, hangup, dtmf, record_stop). The "unknown"
	// bucket catches a future telephony enum addition that this
	// package has not yet been taught about.
	EventsReceived *prometheus.CounterVec

	// EventsDropped counts events the translator declined to forward
	// to the dialer's handler (unbridge / dtmf / record_stop today).
	// Same {type} label set as EventsReceived; the difference between
	// the two counters is the events that DID reach the handler.
	EventsDropped *prometheus.CounterVec

	// EventsTranslationErrors counts the (rare) cases the translator
	// detected an inconsistency in the incoming event (e.g. an
	// unrecognised ChannelEventType). Single series — high signal,
	// low cardinality.
	EventsTranslationErrors prometheus.Counter
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
		panic("router.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		Dials: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_router_dials_total",
				Help: "Total Router.Dial invocations, by result (ok|error).",
			},
			[]string{"result"},
		),
		Hangups: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_router_hangups_total",
				Help: "Total Router.Hangup invocations, by result (ok|error).",
			},
			[]string{"result"},
		),
		EventsReceived: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_router_events_received_total",
				Help: "Total telephony channel events observed by the dialer Router, by raw telephony event type.",
			},
			[]string{"type"},
		),
		EventsDropped: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_router_events_dropped_total",
				Help: "Total telephony channel events the dialer Router dropped (no dialer-side projection), by raw telephony event type.",
			},
			[]string{"type"},
		),
		EventsTranslationErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "dialer_router_events_translation_errors_total",
				Help: "Total telephony channel events the dialer Router could not translate (e.g. unrecognised ChannelEventType).",
			},
		),
	}
	reg.MustRegister(
		m.Dials,
		m.Hangups,
		m.EventsReceived,
		m.EventsDropped,
		m.EventsTranslationErrors,
	)
	return m
}

// Result label constants. Defined as named values so the call sites stay
// typo-resistant and the cardinality bound is enforced at compile time.
const (
	resultOK    = "ok"
	resultError = "error"
)

// observeDial increments the Dials counter for the given result.
// nil-tolerated so the Router works without metrics in tests.
func (m *Metrics) observeDial(result string) {
	if m == nil || m.Dials == nil {
		return
	}
	m.Dials.WithLabelValues(result).Inc()
}

// observeHangup increments the Hangups counter for the given result.
// nil-tolerated.
func (m *Metrics) observeHangup(result string) {
	if m == nil || m.Hangups == nil {
		return
	}
	m.Hangups.WithLabelValues(result).Inc()
}

// observeEventReceived increments the EventsReceived counter for the
// given raw telephony event type. nil-tolerated.
func (m *Metrics) observeEventReceived(eventType string) {
	if m == nil || m.EventsReceived == nil {
		return
	}
	m.EventsReceived.WithLabelValues(eventType).Inc()
}

// observeEventDropped increments the EventsDropped counter for the
// given raw telephony event type. nil-tolerated.
func (m *Metrics) observeEventDropped(eventType string) {
	if m == nil || m.EventsDropped == nil {
		return
	}
	m.EventsDropped.WithLabelValues(eventType).Inc()
}

// observeTranslationError increments the EventsTranslationErrors
// counter. nil-tolerated.
func (m *Metrics) observeTranslationError() {
	if m == nil || m.EventsTranslationErrors == nil {
		return
	}
	m.EventsTranslationErrors.Inc()
}

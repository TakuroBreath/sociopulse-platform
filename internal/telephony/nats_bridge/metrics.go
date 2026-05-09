package nats_bridge //nolint:revive // package name mirrors the module's filesystem path

import "github.com/prometheus/client_golang/prometheus"

// Reasons recorded on CommandsRejected. The set is bounded by design — every
// rejection path in the cmd subscriber maps to exactly one of these strings.
// Adding a new reason requires extending the list AND the comment on
// CommandsRejected so dashboards stay in sync.
const (
	rejectReasonDuplicate        = "duplicate"
	rejectReasonMalformed        = "malformed"
	rejectReasonUnknownKind      = "unknown_kind"
	rejectReasonPoolError        = "pool_error"
	rejectReasonIdempotencyError = "idempotency_error"
)

// Reasons recorded on EventsDropped.
const (
	dropReasonPublishError = "publish_error"
	dropReasonMarshalError = "marshal_error"
)

// Command kinds recorded on CommandsReceived. Plain strings used as label
// values; defined as constants so handle() and metric instrumentation stay
// in lockstep.
const (
	kindOriginate       = "originate"
	kindHangup          = "hangup"
	kindMixmonitorStart = "mixmonitor.start"
	kindMixmonitorStop  = "mixmonitor.stop"
)

// Metrics groups the Prometheus collectors emitted by the nats bridge.
// Construction is gated behind RegisterMetrics so the composition root can
// attach a scoped *prometheus.Registry — no init()-time MustRegister,
// matching the Plan 09/10 carry-forward rule.
type Metrics struct {
	// CommandsReceived counts every command envelope the bridge consumed
	// from the bus, partitioned by kind ∈ {originate, hangup,
	// mixmonitor.start, mixmonitor.stop}. Bounded label set.
	CommandsReceived *prometheus.CounterVec

	// CommandsRejected counts commands the bridge declined to dispatch to
	// the ESL pool. Bounded labels: reason ∈ {duplicate, malformed,
	// unknown_kind, pool_error, idempotency_error}. A non-zero rate of
	// idempotency_error / pool_error is the canary for downstream trouble.
	CommandsRejected *prometheus.CounterVec

	// EventsPublished counts ESL events the bridge fanned out to NATS,
	// partitioned by kind (the api.ChannelEventType string — bounded enum).
	EventsPublished *prometheus.CounterVec

	// EventsDropped counts events that fell out of the pipeline before
	// reaching NATS. Bounded labels: reason ∈ {publish_error,
	// marshal_error}.
	EventsDropped *prometheus.CounterVec
}

// RegisterMetrics builds a fresh *Metrics and registers every collector on
// the supplied registerer. The caller owns the registerer's lifetime —
// production wiring uses pkg/observability.Metrics.Registry; tests pass
// prometheus.NewRegistry().
//
// reg must be non-nil; a nil registerer is a wiring bug and panics here so
// the failure surfaces at boot rather than at first metric emission. Panics
// also on duplicate registration for the same reason.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("nats_bridge.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		CommandsReceived: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telephony_bridge_commands_received_total",
				Help: "Total command envelopes consumed by the nats_bridge cmd subscriber, by kind.",
			},
			[]string{"kind"},
		),
		CommandsRejected: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telephony_bridge_commands_rejected_total",
				Help: "Total commands the bridge declined to dispatch, by reason (duplicate, malformed, unknown_kind, pool_error, idempotency_error).",
			},
			[]string{"reason"},
		),
		EventsPublished: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telephony_bridge_events_published_total",
				Help: "Total ESL channel events the bridge published to NATS, by event kind.",
			},
			[]string{"kind"},
		),
		EventsDropped: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telephony_bridge_events_dropped_total",
				Help: "Total events the bridge dropped before successful publish, by reason (publish_error, marshal_error).",
			},
			[]string{"reason"},
		),
	}
	reg.MustRegister(
		m.CommandsReceived,
		m.CommandsRejected,
		m.EventsPublished,
		m.EventsDropped,
	)
	return m
}

// observeCommandReceived ticks CommandsReceived for the named kind. nil
// receiver is tolerated so subsystems wired without metrics keep working.
func (m *Metrics) observeCommandReceived(kind string) {
	if m == nil || m.CommandsReceived == nil {
		return
	}
	m.CommandsReceived.WithLabelValues(kind).Inc()
}

// observeCommandRejected ticks CommandsRejected for the named reason.
func (m *Metrics) observeCommandRejected(reason string) {
	if m == nil || m.CommandsRejected == nil {
		return
	}
	m.CommandsRejected.WithLabelValues(reason).Inc()
}

// observeEventPublished ticks EventsPublished for the event kind.
func (m *Metrics) observeEventPublished(kind string) {
	if m == nil || m.EventsPublished == nil {
		return
	}
	m.EventsPublished.WithLabelValues(kind).Inc()
}

// observeEventDropped ticks EventsDropped for the reason.
func (m *Metrics) observeEventDropped(reason string) {
	if m == nil || m.EventsDropped == nil {
		return
	}
	m.EventsDropped.WithLabelValues(reason).Inc()
}

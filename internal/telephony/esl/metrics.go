package esl

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by this package. The
// fields stay exported so test code can poke them; production callers
// receive a *Metrics from RegisterMetrics and pass it into Config.Metrics.
//
// The package deliberately does NOT register collectors at init() time.
// Two test imports of a single-init() package collide on
// prometheus.DefaultRegisterer; instead the composition root passes a
// scoped *prometheus.Registry into RegisterMetrics, and each test that
// exercises metrics builds its own fresh registry.
type Metrics struct {
	// Connected is 1 when the client has a healthy ESL connection to the
	// labelled node, 0 otherwise. Gauge per node.
	Connected *prometheus.GaugeVec

	// CommandsTotal counts every ESL command issued, partitioned by node,
	// command verb, and result ("ok" / "err" / "timeout"). The verb is
	// the first whitespace-delimited token of the command line — high
	// enough cardinality to be useful without exploding the series count.
	CommandsTotal *prometheus.CounterVec

	// CommandDuration is the wall-clock latency of every issued command.
	// Histogram per node + verb (the result is intentionally NOT a label
	// here — it's already on CommandsTotal).
	CommandDuration *prometheus.HistogramVec

	// EventsTotal counts every event surfaced on the Events() channel,
	// partitioned by node and event-name (CHANNEL_CREATE, …).
	EventsTotal *prometheus.CounterVec

	// ReconnectsTotal counts reconnect attempts, partitioned by node and
	// outcome ("ok" / "err"). Higher layers (Task 4 supervisor) update
	// this; the client itself only emits success/failure on the initial
	// Dial.
	ReconnectsTotal *prometheus.CounterVec
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
		panic("esl.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		Connected: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "esl_connected",
				Help: "1 if the ESL connection to the labelled node is up, 0 otherwise.",
			},
			[]string{"node"},
		),
		CommandsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "esl_commands_total",
				Help: "Total ESL commands sent, by node, command verb, and result.",
			},
			[]string{"node", "command", "result"},
		),
		CommandDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "esl_command_duration_seconds",
				Help:    "Wall-clock latency of ESL commands, by node and verb.",
				Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
			},
			[]string{"node", "command"},
		),
		EventsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "esl_events_total",
				Help: "Total ESL events received, by node and event name.",
			},
			[]string{"node", "event"},
		),
		ReconnectsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "esl_reconnects_total",
				Help: "Total ESL reconnect attempts, by node and result.",
			},
			[]string{"node", "result"},
		),
	}
	reg.MustRegister(
		m.Connected,
		m.CommandsTotal,
		m.CommandDuration,
		m.EventsTotal,
		m.ReconnectsTotal,
	)
	return m
}

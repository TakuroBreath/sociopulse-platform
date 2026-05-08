package eventbus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics groups the Prometheus collectors emitted by the JetStream
// publisher + subscriber implementations. Construction is gated
// behind RegisterMetrics so the composition root attaches a scoped
// *prometheus.Registry — no init()-time MustRegister, matching the
// Plan 09/10 carry-forward rule.
type Metrics struct {
	// PublishTotal counts publish attempts by outcome ("ok", "error").
	// Bounded label set; safe for high-cardinality dashboards.
	PublishTotal *prometheus.CounterVec

	// PublishLatency observes the wall-clock duration of synchronous
	// PublishMsg calls (broker round-trip including ack). Buckets are
	// tuned for healthy-cluster p99 under 50ms; the long tail (1s+)
	// is captured for outlier alerting.
	PublishLatency prometheus.Histogram

	// SubscribeMessageTotal counts inbound JetStream messages by ack
	// outcome ("ack" = handler returned nil, "nak" = handler returned
	// error and we NAK'd for redelivery).
	SubscribeMessageTotal *prometheus.CounterVec

	// SubscriberRedeliveries counts redeliveries scheduled by NAK.
	// Not the same as SubscribeMessageTotal{result="nak"} once you
	// add multi-deliver-attempt streams: a NAK schedules ONE
	// redelivery; the same message-id can NAK again. This counter
	// tracks the cumulative (re)deliveries.
	SubscriberRedeliveries prometheus.Counter
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
		panic("eventbus.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		PublishTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "eventbus_publish_total",
				Help: "Total JetStream publish attempts, by result (ok|error).",
			},
			[]string{"result"},
		),
		PublishLatency: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name: "eventbus_publish_latency_seconds",
				Help: "Synchronous JetStream publish latency in seconds (broker ack round-trip).",
				// Buckets aligned to a healthy-cluster p99 < 50ms
				// expectation, with a tail out to 5s for outlier
				// alerting. The default DefBuckets [.005..10] spends
				// most of its resolution above 100ms, where almost
				// no real publish samples land.
				Buckets: []float64{
					0.0005, 0.001, 0.0025, 0.005,
					0.01, 0.025, 0.05, 0.1,
					0.25, 0.5, 1, 2.5, 5,
				},
			},
		),
		SubscribeMessageTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "eventbus_subscribe_message_total",
				Help: "Total inbound JetStream messages, by ack outcome (ack|nak).",
			},
			[]string{"result"},
		),
		SubscriberRedeliveries: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "eventbus_subscriber_redeliveries_total",
				Help: "Total redeliveries scheduled by NAK (handler returned error).",
			},
		),
	}
	reg.MustRegister(
		m.PublishTotal,
		m.PublishLatency,
		m.SubscribeMessageTotal,
		m.SubscriberRedeliveries,
	)
	return m
}

// observePublish records a publish attempt and its latency.
// nil-tolerated so callers without metrics keep working.
func (m *Metrics) observePublish(result string, dur time.Duration) {
	if m == nil {
		return
	}
	if m.PublishTotal != nil {
		m.PublishTotal.WithLabelValues(result).Inc()
	}
	if m.PublishLatency != nil {
		m.PublishLatency.Observe(dur.Seconds())
	}
}

// observeSubscribeMessage records an inbound message's ack outcome.
// nil-tolerated.
func (m *Metrics) observeSubscribeMessage(result string) {
	if m == nil || m.SubscribeMessageTotal == nil {
		return
	}
	m.SubscribeMessageTotal.WithLabelValues(result).Inc()
}

// observeRedelivery records a NAK-driven redelivery scheduling.
// nil-tolerated.
func (m *Metrics) observeRedelivery() {
	if m == nil || m.SubscriberRedeliveries == nil {
		return
	}
	m.SubscriberRedeliveries.Inc()
}

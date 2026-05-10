package events

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics groups the Prometheus collectors emitted by the realtime
// dispatcher. Construction is gated behind RegisterMetrics so the
// composition root can attach a scoped *prometheus.Registry — no
// init()-time MustRegister, matching the Plan 09/10 carry-forward rule.
type Metrics struct {
	// MessagesTotal counts inbound NATS messages dispatched to the
	// Hub, partitioned by topic. Bounded label set (topic enum).
	MessagesTotal *prometheus.CounterVec

	// DispatchFailures counts messages skipped before they reached the
	// Hub. Bounded labels: topic + reason ∈ {"malformed_subject",
	// "empty_tenant", "tenant_lister_failed"}. A non-zero rate indicates
	// either a misconfigured upstream publisher (wrong subject token
	// count), a defence-in-depth hit (broker delivered an empty-tenant
	// subject), or a *TrunksReplicator tenant-catalog lookup failure.
	DispatchFailures *prometheus.CounterVec

	// FanoutSize is the distribution of Hub.Broadcast return values —
	// i.e. the per-message recipient count. Useful for spotting
	// subscriber-leakage regressions (mean drops to 0) or runaway
	// subscription growth (tail spikes).
	FanoutSize prometheus.Histogram
}

// Reasons recorded on DispatchFailures. The set is bounded by design —
// every failure path in the dispatcher pipeline maps to exactly one of
// these three string constants. Adding a new reason requires updating
// the DispatchFailures comment above so dashboards stay in sync.
const (
	reasonMalformed          = "malformed_subject"
	reasonEmptyTenant        = "empty_tenant"
	reasonTenantListerFailed = "tenant_lister_failed"
)

// RegisterMetrics builds a fresh *Metrics and registers every collector
// on reg. The caller owns the registerer's lifetime — production wiring
// uses pkg/observability.Metrics.Registry; tests pass
// prometheus.NewRegistry().
//
// reg must be non-nil; a nil registerer is a wiring bug and panics here
// so failure surfaces at boot, not at first metric emission.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("events.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		MessagesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_dispatcher_messages_total",
				Help: "Total NATS messages dispatched into the realtime Hub, by topic.",
			},
			[]string{"topic"},
		),
		DispatchFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_dispatcher_dispatch_failures_total",
				Help: "Total dispatcher skip events before Hub.Broadcast, by topic and reason.",
			},
			[]string{"topic", "reason"},
		),
		FanoutSize: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "realtime_dispatcher_fanout_size",
				Help:    "Distribution of Hub.Broadcast recipient counts per dispatched message.",
				Buckets: []float64{0, 1, 2, 5, 10, 20, 50, 100, 200},
			},
		),
	}
	reg.MustRegister(
		m.MessagesTotal,
		m.DispatchFailures,
		m.FanoutSize,
	)
	return m
}

// observeMessage increments MessagesTotal for topic. nil-tolerated so
// dispatchers without metrics keep working.
func (m *Metrics) observeMessage(topic string) {
	if m == nil || m.MessagesTotal == nil {
		return
	}
	m.MessagesTotal.WithLabelValues(topic).Inc()
}

// observeDispatchFailure increments DispatchFailures for the given
// (topic, reason) tuple. nil-tolerated.
func (m *Metrics) observeDispatchFailure(topic, reason string) {
	if m == nil || m.DispatchFailures == nil {
		return
	}
	m.DispatchFailures.WithLabelValues(topic, reason).Inc()
}

// observeFanout records one fan-out recipient count sample. nil-tolerated.
func (m *Metrics) observeFanout(count int) {
	if m == nil || m.FanoutSize == nil {
		return
	}
	m.FanoutSize.Observe(float64(count))
}

// CacheInvalidatorMetrics is the per-handler counter set surfaced
// on /metrics for *CacheInvalidator. Plan 11.4 Task 6 expanded the
// label set from {result} alone to {subject, result} — the same
// counter family covers all three subscription dimensions.
//
// Bounded label combinations:
//   - subject ∈ {SubjectProjectStatus, SubjectUserDeleted, SubjectRecordingCallDeleted}
//   - result ∈ {"ok", "parse_error", "empty_id"}
//
// 9 cells total. The "empty_project_id" label value used in Plan 11.3
// Task 3 is renamed to "empty_id" — uniform across subjects. Operators
// updating dashboards: the previous query read by result alone; after
// the bump query by subject="tenant.*.crm.project.status_changed".
//
// Nil-tolerated — observe is a no-op on a nil receiver, matching the
// rest of the realtime metrics types. Separated from *Metrics so a
// future invalidator that doesn't share the dispatcher's lifecycle
// can be wired/un-wired independently (e.g. a degraded boot that
// opts out of the JetStream subscriber would still need the rest
// of *Metrics).
type CacheInvalidatorMetrics struct {
	// invalidations counts dispatched invalidation messages,
	// labelled by (subject, result). Bounded label set — every
	// code path in CacheInvalidator.handle{Project,User,Call}
	// maps to exactly one (subject, result) cell.
	invalidations *prometheus.CounterVec
}

// RegisterCacheInvalidatorMetrics builds a fresh
// *CacheInvalidatorMetrics and registers its counter on reg. The
// caller owns the registerer's lifetime — cmd/api wires
// pkg/observability.Metrics.Registry; tests pass
// prometheus.NewRegistry().
//
// reg must be non-nil; a nil registerer is a wiring bug and panics
// here so failure surfaces at boot, not at first metric emission
// (mirrors RegisterMetrics' contract above).
func RegisterCacheInvalidatorMetrics(reg prometheus.Registerer) *CacheInvalidatorMetrics {
	if reg == nil {
		panic("events.RegisterCacheInvalidatorMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &CacheInvalidatorMetrics{
		invalidations: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_cache_invalidations_total",
				Help: "Number of resolver-cache invalidations dispatched, labelled by subject and outcome (ok / parse_error / empty_id).",
			},
			[]string{"subject", "result"},
		),
	}
	reg.MustRegister(m.invalidations)
	return m
}

// observe ticks the (subject, result)-labelled counter. nil-safe so
// callers with no metrics wired (tests / degraded boot) keep working.
func (m *CacheInvalidatorMetrics) observe(subject, result string) {
	if m == nil || m.invalidations == nil {
		return
	}
	m.invalidations.WithLabelValues(subject, result).Inc()
}

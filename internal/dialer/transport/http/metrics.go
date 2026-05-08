package http

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by the dialer HTTP
// transport. Per Plan 09 carry-forward, this package deliberately does
// NOT register collectors at init() time — two test imports of an init()-
// registering package collide on prometheus.DefaultRegisterer. Instead
// the composition root passes a scoped *prometheus.Registry into
// RegisterMetrics, and each test that exercises metrics builds its own
// fresh registry.
//
// All observe* helpers tolerate a nil receiver so the transport works
// without metrics in unit tests (Deps.Metrics is optional).
type Metrics struct {
	// PresenceRefreshFailures counts every fsm.RefreshPresence error
	// surfaced through RefreshPresenceMiddleware. Redis-down is the
	// dominant contributor; tenant-scoping is intentionally omitted —
	// the underlying Redis connection isn't tenant-aware so a label
	// would inflate cardinality without adding signal.
	PresenceRefreshFailures prometheus.Counter
}

// RegisterMetrics builds a fresh *Metrics and registers every collector
// on the supplied registerer. The caller owns the registerer's lifetime
// — production wires metrics.Registry from pkg/observability; tests pass
// prometheus.NewRegistry().
//
// reg must be non-nil; a nil registerer is a wiring error and panics here
// so the failure surfaces at boot instead of at first metric emission.
// Panics also on duplicate registration for the same reason.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("dialer/transport/http.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		PresenceRefreshFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dialer_presence_refresh_failures_total",
			Help: "Total RefreshPresence errors observed by the operator-route middleware.",
		}),
	}
	reg.MustRegister(m.PresenceRefreshFailures)
	return m
}

// observePresenceRefreshFailure increments the failure counter. nil-
// tolerated so the middleware works without metrics in unit tests.
func (m *Metrics) observePresenceRefreshFailure() {
	if m == nil || m.PresenceRefreshFailures == nil {
		return
	}
	m.PresenceRefreshFailures.Inc()
}

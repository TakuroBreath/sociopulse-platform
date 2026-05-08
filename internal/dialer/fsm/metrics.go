package fsm

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// Metrics groups the Prometheus collectors emitted by the FSM. Fields are
// exported so test code can poke them directly; production callers
// receive a *Metrics from RegisterMetrics and pass it into Config.Metrics.
//
// Like Plan 09's package-private metrics, this package deliberately does
// NOT register collectors at init() time. Two test imports of a single-
// init() package collide on prometheus.DefaultRegisterer; instead the
// composition root passes a scoped *prometheus.Registry into
// RegisterMetrics, and each test that exercises metrics builds its own
// fresh registry.
type Metrics struct {
	// Transitions counts every successful FSM transition, partitioned
	// by from-state, to-state, and event. High-cardinality but
	// bounded (7 states × 12 events = 84 labels max).
	Transitions *prometheus.CounterVec

	// InvalidTransitions counts transitions rejected as invalid by the
	// transition table. Operator UI bugs surface here as a steady
	// non-zero rate on a particular (from, event) pair.
	InvalidTransitions *prometheus.CounterVec

	// Force counts every Force() invocation, partitioned by target state
	// and reason ("heartbeat_lost", "supervisor_kick", ...). Spikes on
	// the heartbeat_lost reason indicate flaky operator-side networking.
	Force *prometheus.CounterVec
}

// RegisterMetrics builds a fresh *Metrics and registers every collector
// on the supplied registerer. The caller owns the registerer's lifetime
// — a typical wiring is metrics.Registry from pkg/observability, but
// tests pass prometheus.NewRegistry().
//
// reg must be non-nil; a nil registerer is a wiring error and panics
// here so the failure surfaces at boot instead of at first metric
// emission. Panics also on duplicate registration for the same reason.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("fsm.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		Transitions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_fsm_transitions_total",
				Help: "Total successful operator FSM transitions, by from-state, to-state, and event.",
			},
			[]string{"from", "to", "event"},
		),
		InvalidTransitions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_fsm_invalid_transitions_total",
				Help: "Total operator FSM transitions rejected as invalid by the transition table.",
			},
			[]string{"from", "event"},
		),
		Force: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_fsm_force_total",
				Help: "Total Force() invocations on the operator FSM, by target state and reason.",
			},
			[]string{"target", "reason"},
		),
	}
	reg.MustRegister(
		m.Transitions,
		m.InvalidTransitions,
		m.Force,
	)
	return m
}

// observeTransition increments the Transitions counter for the given
// edge. nil-tolerated so the FSM works without metrics in tests.
func (m *Metrics) observeTransition(from, to api.State, evt api.Event) {
	if m == nil || m.Transitions == nil {
		return
	}
	m.Transitions.WithLabelValues(string(from), string(to), string(evt)).Inc()
}

// observeInvalid increments the InvalidTransitions counter. nil-tolerated.
func (m *Metrics) observeInvalid(from api.State, evt api.Event) {
	if m == nil || m.InvalidTransitions == nil {
		return
	}
	m.InvalidTransitions.WithLabelValues(string(from), string(evt)).Inc()
}

// observeForce increments the Force counter. nil-tolerated.
func (m *Metrics) observeForce(target api.State, reason string) {
	if m == nil || m.Force == nil {
		return
	}
	m.Force.WithLabelValues(string(target), reason).Inc()
}

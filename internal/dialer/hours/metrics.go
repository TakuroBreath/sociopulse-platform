package hours

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups the Prometheus collectors emitted by the hours
// Checker. Per Plan 09 lessons (and matching the rest of the dialer
// packages), this package deliberately does NOT register collectors
// at init() time. Two test imports of an init()-registering package
// collide on prometheus.DefaultRegisterer — the composition root
// passes a scoped registerer into RegisterMetrics instead.
//
// All observe* helpers tolerate a nil receiver so the Checker works
// without metrics in unit tests.
type Metrics struct {
	// Checks counts every IsAllowed invocation, partitioned by the
	// terminal result. Cardinality is bounded (5 series): "allowed",
	// "denied" (generic outside-window), "holiday" (federal RU
	// holiday), "outside_window" (default / tenant window denied),
	// "error" (region unknown / settings transport / parse error).
	//
	// "denied" subsumes the holiday + outside_window buckets so a
	// dashboard can count "all closed-decisions" with a single
	// aggregate; the more specific labels surface WHY for alerting
	// and tuning.
	Checks *prometheus.CounterVec
}

// RegisterMetrics builds a fresh *Metrics and registers every
// collector on the supplied registerer. The caller owns the
// registerer's lifetime — production wires metrics.Registry from
// pkg/observability; tests pass prometheus.NewRegistry().
//
// reg must be non-nil; a nil registerer is a wiring bug and panics
// here so the failure surfaces at boot instead of at first metric
// emission. Panics also on duplicate registration for the same
// reason.
func RegisterMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		panic("hours.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &Metrics{
		Checks: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "dialer_hours_check_total",
				Help: "Total WorkingHoursChecker.IsAllowed invocations, by result (allowed|denied|holiday|outside_window|error).",
			},
			[]string{"result"},
		),
	}
	reg.MustRegister(m.Checks)
	return m
}

// Result label constants. Defined as named values so call sites stay
// typo-resistant and the cardinality bound is enforced at compile
// time.
const (
	resultAllowed       = "allowed"
	resultDenied        = "denied"
	resultHoliday       = "holiday"
	resultOutsideWindow = "outside_window"
	resultError         = "error"
)

// observeCheck increments the Checks counter for the given result.
// nil-tolerated so the Checker works without metrics in tests.
func (m *Metrics) observeCheck(result string) {
	if m == nil || m.Checks == nil {
		return
	}
	m.Checks.WithLabelValues(result).Inc()
}

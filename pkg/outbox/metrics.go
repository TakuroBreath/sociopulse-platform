package outbox

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// RelayMetrics aggregates Prometheus collectors for the outbox relay.
// A nil receiver is safe — every helper short-circuits, mirroring the
// nil-safety carry-forward from internal/analytics/metrics.
//
// Cardinality discipline: ParkedRows carries a single tenant label.
// Tenant count is bounded by Plan 00a's 30-tenant target; the
// DeleteLabelValues path in the relay's DLQ poll prunes tenants whose
// parked count drops to zero, so the series count tracks the active
// "tenants with parked rows" set rather than the global tenant inventory.
type RelayMetrics struct {
	// ParkedRows is the number of unpublished event_outbox rows whose
	// attempts column has reached MaxRetry, per tenant. Operators alert
	// on `sociopulse_outbox_parked_rows > 0 for 5m` and remediate by
	// inspecting last_error / restarting the relay after fixing the
	// downstream NATS subject.
	//
	// Gauge (not counter): parked rows can decrease (manual remediation
	// resets attempts). A counter would lie when operators clear the
	// backlog.
	ParkedRows *prometheus.GaugeVec // labels: tenant
}

// RegisterRelayMetrics constructs and (optionally) registers all relay
// collectors with reg. Returns the populated struct and a non-nil error
// if any registration fails. reg may be nil — collectors are still
// constructed but not registered (useful in unit tests that don't run a
// Prometheus server).
func RegisterRelayMetrics(reg prometheus.Registerer) (*RelayMetrics, error) {
	m := &RelayMetrics{
		ParkedRows: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "sociopulse",
			Subsystem: "outbox",
			Name:      "parked_rows",
			Help:      "Outbox rows parked at attempts >= MaxRetry; awaiting manual remediation. Alert: > 0 for 5m.",
		}, []string{"tenant"}),
	}

	if reg == nil {
		return m, nil
	}

	for _, c := range []prometheus.Collector{m.ParkedRows} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("outbox: register metrics: %w", err)
		}
	}
	return m, nil
}

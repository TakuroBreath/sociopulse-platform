package outbox_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/outbox"
)

// TestRegisterRelayMetrics_RegistersAndExposesParkedRows verifies the
// constructor wires the gauge into the supplied registry and that the
// metric is queryable with a tenant label.
func TestRegisterRelayMetrics_RegistersAndExposesParkedRows(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := outbox.RegisterRelayMetrics(reg)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.NotNil(t, m.ParkedRows)

	const tenant = "11111111-1111-1111-1111-111111111111"
	m.ParkedRows.WithLabelValues(tenant).Set(5)
	require.InDelta(t, 5.0, testutil.ToFloat64(m.ParkedRows.WithLabelValues(tenant)), 0.0)
}

// TestRegisterRelayMetrics_NilRegistererIsSafe ensures the constructor
// returns a populated struct even with nil reg (unit-test convenience).
func TestRegisterRelayMetrics_NilRegistererIsSafe(t *testing.T) {
	t.Parallel()

	m, err := outbox.RegisterRelayMetrics(nil)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.NotNil(t, m.ParkedRows)
}

// TestRegisterRelayMetrics_DuplicateRegistrationErrors verifies double
// registration on the same registry fails loudly. This catches misuse
// at composition time rather than silently masking metrics.
func TestRegisterRelayMetrics_DuplicateRegistrationErrors(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	_, err := outbox.RegisterRelayMetrics(reg)
	require.NoError(t, err)

	_, err = outbox.RegisterRelayMetrics(reg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "outbox",
		"error must mention outbox to aid triage")
}

// TestRelayMetrics_ParkedRowsMetricMetadata pins the metric name and help
// text so an accidental rename triggers a test failure. Operators write
// alert rules against this exact name.
func TestRelayMetrics_ParkedRowsMetricMetadata(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := outbox.RegisterRelayMetrics(reg)
	require.NoError(t, err)

	const want = `
		# HELP sociopulse_outbox_parked_rows Outbox rows parked at attempts >= MaxRetry; awaiting manual remediation. Alert: > 0 for 5m.
		# TYPE sociopulse_outbox_parked_rows gauge
		sociopulse_outbox_parked_rows{tenant="t1"} 3
	`
	m.ParkedRows.WithLabelValues("t1").Set(3)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(want), "sociopulse_outbox_parked_rows"))
}

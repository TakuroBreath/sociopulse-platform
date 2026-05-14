package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/analytics/metrics"
)

// TestRegisterIngestMetrics_NilReg asserts the constructor tolerates a
// nil registry — the collectors are still constructed (so the Inc*
// helpers do not no-op silently in production wired without /metrics).
func TestRegisterIngestMetrics_NilReg(t *testing.T) {
	t.Parallel()
	m, err := metrics.RegisterIngestMetrics(nil)
	require.NoError(t, err)
	require.NotNil(t, m)
}

// TestRegisterIngestMetrics_DuplicateFails asserts the second
// registration on the same registry returns an error. Mirrors the
// recording-module convention — registries are owned by the composition
// root and double-registration is a wiring bug we want loud.
func TestRegisterIngestMetrics_DuplicateFails(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	_, err := metrics.RegisterIngestMetrics(reg)
	require.NoError(t, err)
	_, err = metrics.RegisterIngestMetrics(reg)
	require.Error(t, err, "second registration must fail")
}

// TestIngestMetrics_NilReceiverNoOp asserts every Inc*/Observe* helper
// is safe to call on a nil receiver. The IngestPipeline tolerates a
// nil *IngestMetrics so test wiring can skip the collector setup.
func TestIngestMetrics_NilReceiverNoOp(t *testing.T) {
	t.Parallel()
	var m *metrics.IngestMetrics
	require.NotPanics(t, func() {
		m.IncReceived("analytics.event.calls")
		m.IncInserted("analytics.event.calls", 10)
		m.IncFailed("analytics.event.calls", "send")
		m.IncDeadLetter("analytics.event.calls")
		m.IncDedupHit("analytics.event.calls")
		m.ObserveBatchSize("analytics.event.calls", 100)
		m.ObserveFlushLatency("analytics.event.calls", 0.05)
	})
}

// TestIngestMetrics_CounterIncrement asserts the Inc* helpers actually
// move the underlying counters. The exact label tuple is bounded to
// the three declared analytics subjects + 1 fail reason — Prometheus
// label cardinality stays small.
func TestIngestMetrics_CounterIncrement(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m, err := metrics.RegisterIngestMetrics(reg)
	require.NoError(t, err)

	m.IncReceived("analytics.event.calls")
	m.IncReceived("analytics.event.calls")
	m.IncReceived("analytics.event.operator_state")
	m.IncDedupHit("analytics.event.calls")
	m.IncFailed("analytics.event.calls", "prepare_batch")
	m.IncDeadLetter("analytics.event.calls")
	m.IncInserted("analytics.event.calls", 5)
	m.ObserveBatchSize("analytics.event.calls", 5)
	m.ObserveFlushLatency("analytics.event.calls", 0.123)

	got := counterValue(t, reg, "sociopulse_analytics_ingest_received_total", "analytics.event.calls")
	require.InDelta(t, 2.0, got, 0.0001)

	got = counterValue(t, reg, "sociopulse_analytics_ingest_received_total", "analytics.event.operator_state")
	require.InDelta(t, 1.0, got, 0.0001)

	got = counterValue(t, reg, "sociopulse_analytics_ingest_dedup_hits_total", "analytics.event.calls")
	require.InDelta(t, 1.0, got, 0.0001)

	got = counterValue(t, reg, "sociopulse_analytics_ingest_failed_total", "analytics.event.calls")
	require.InDelta(t, 1.0, got, 0.0001)

	got = counterValue(t, reg, "sociopulse_analytics_ingest_dead_letter_total", "analytics.event.calls")
	require.InDelta(t, 1.0, got, 0.0001)

	got = counterValue(t, reg, "sociopulse_analytics_ingest_inserted_total", "analytics.event.calls")
	require.InDelta(t, 5.0, got, 0.0001)

	require.Equal(t, uint64(1), histogramSampleCount(t, reg, "sociopulse_analytics_ingest_batch_size", "analytics.event.calls"))
	require.Equal(t, uint64(1), histogramSampleCount(t, reg, "sociopulse_analytics_ingest_flush_latency_seconds", "analytics.event.calls"))
}

// counterValue gathers the metric `name` from reg and returns the value
// of the timeseries whose "subject" label matches subjectVal. Fatals
// when not present. The label name is always "subject" for this
// package's metrics, so it is fixed here rather than parameterised.
func counterValue(t *testing.T, reg *prometheus.Registry, name, subjectVal string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, mp := range fam.GetMetric() {
			for _, lp := range mp.GetLabel() {
				if lp.GetName() == "subject" && lp.GetValue() == subjectVal {
					return mp.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("counter %q with subject=%s not found", name, subjectVal)
	return 0
}

// histogramSampleCount returns the cumulative observation count for the
// named histogram in reg, filtered by the "subject" label value.
func histogramSampleCount(t *testing.T, reg *prometheus.Registry, name, subjectVal string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, mp := range fam.GetMetric() {
			for _, lp := range mp.GetLabel() {
				if lp.GetName() == "subject" && lp.GetValue() == subjectVal {
					return mp.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	t.Fatalf("histogram %q with subject=%s not found", name, subjectVal)
	return 0
}

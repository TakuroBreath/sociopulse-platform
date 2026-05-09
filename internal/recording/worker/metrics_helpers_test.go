//go:build integration

package worker_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// counterValue returns the current value of a CounterVec child cell
// addressed by labelValues. Returns 0 if the cell hasn't been touched
// yet (GetMetricWithLabelValues lazily creates a zero-valued child for
// the requested label tuple — no panic on missing children, unlike
// prometheus/testutil.ToFloat64 which panics on a non-Counter input).
//
// Tests use this helper to assert metric labels actually ticked, not
// just that side-effect state changed — silent regressions in the
// label set (e.g. accidentally sending "stale" instead of "orphaned")
// fail loudly here.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labelValues ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

// histogramSampleCount returns the total observation count of a
// HistogramVec child cell. Returns 0 if the cell hasn't been touched.
// Used to assert that a Run-loop tick actually observed a duration
// sample (so a hung daemon vs. a working-but-quiet daemon are
// distinguishable).
func histogramSampleCount(t *testing.T, vec *prometheus.HistogramVec, labelValues ...string) uint64 {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	pm, ok := obs.(prometheus.Metric)
	require.True(t, ok, "histogram observer must satisfy prometheus.Metric")
	var m dto.Metric
	require.NoError(t, pm.Write(&m))
	return m.GetHistogram().GetSampleCount()
}

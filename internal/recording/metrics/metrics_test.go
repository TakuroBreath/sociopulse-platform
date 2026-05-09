package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/metrics"
)

func TestRegisterRecordingMetrics_NilReg(t *testing.T) {
	t.Parallel()
	m, err := metrics.RegisterRecordingMetrics(nil)
	require.NoError(t, err)
	require.NotNil(t, m)
}

func TestRegisterRecordingMetrics_DuplicateFails(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	_, err := metrics.RegisterRecordingMetrics(reg)
	require.NoError(t, err)
	_, err = metrics.RegisterRecordingMetrics(reg)
	require.Error(t, err, "second registration must fail")
}

func TestRecordingMetrics_NilReceiverNoOp(t *testing.T) {
	t.Parallel()
	var m *metrics.RecordingMetrics
	require.NotPanics(t, func() {
		m.ObserveCommit("t", "ok", 0.1)
		m.AddStorageBytes("t", 1234)
		m.ObserveAccess("t", "ok", 0.1)
		m.ObserveRetentionPass("cold_move", "ok", 0.1)
		m.IncRetentionAction("t", "cold_move", "ok")
		m.ObserveIntegrityPass("ok", 0.1)
		m.IncIntegrityAction("t", "ok")
		m.IncIntegrityFailure("t")
		m.SetLeaderActive("retention", true)
		m.SetLeaderActive("integrity", false)
	})
}

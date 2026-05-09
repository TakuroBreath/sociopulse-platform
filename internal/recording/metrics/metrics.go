// Package metrics owns Prometheus collectors for the recording module.
// Constructors return errors on duplicate registration — no init() / no MustRegister.
// Following Plans 09/10/11 carry-forward: every metrics struct must support
// nil-safe usage so unit tests can pass nil where a metric tick is irrelevant.
package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// RecordingMetrics aggregates Prometheus collectors for the recording module.
// A nil receiver is safe — every method becomes a no-op.
type RecordingMetrics struct {
	CommitTotal      *prometheus.CounterVec   // labels: tenant_id, result {ok|replay|invalid|call_not_found|error}
	StorageSizeBytes *prometheus.GaugeVec     // labels: tenant_id (Counter-like, only Add on commit)
	CommitDuration   *prometheus.HistogramVec // labels: tenant_id, result
}

// RegisterRecordingMetrics constructs and registers all collectors with reg.
// Returns the populated struct + a non-nil error if any registration fails.
// Reg may be nil — in that case the collectors are still constructed but not
// registered (useful in unit tests that don't run a Prometheus server).
func RegisterRecordingMetrics(reg prometheus.Registerer) (*RecordingMetrics, error) {
	m := &RecordingMetrics{
		CommitTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "commit_total",
			Help:      "Number of RecordingService.Commit calls broken out by result.",
		}, []string{"tenant_id", "result"}),

		StorageSizeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "storage_size_bytes",
			Help:      "Cumulative bytes_size of all committed (non-deleted) recordings, by tenant.",
		}, []string{"tenant_id"}),

		CommitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "commit_duration_seconds",
			Help:      "Wall time of one Commit call (validation + INSERT + outbox + audit).",
			Buckets:   prometheus.DefBuckets,
		}, []string{"tenant_id", "result"}),
	}

	if reg == nil {
		return m, nil
	}

	for _, c := range []prometheus.Collector{m.CommitTotal, m.StorageSizeBytes, m.CommitDuration} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("recording metrics: register: %w", err)
		}
	}
	return m, nil
}

// ObserveCommit ticks the relevant collectors. Safe to call on a nil receiver.
func (m *RecordingMetrics) ObserveCommit(tenantID, result string, durSec float64) {
	if m == nil {
		return
	}
	m.CommitTotal.WithLabelValues(tenantID, result).Inc()
	m.CommitDuration.WithLabelValues(tenantID, result).Observe(durSec)
}

// AddStorageBytes records a successful commit's bytes_size. Safe on nil.
func (m *RecordingMetrics) AddStorageBytes(tenantID string, bytes int64) {
	if m == nil || bytes < 0 {
		return
	}
	m.StorageSizeBytes.WithLabelValues(tenantID).Add(float64(bytes))
}

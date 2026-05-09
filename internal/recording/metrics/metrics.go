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
	AccessTotal      *prometheus.CounterVec   // labels: tenant_id, result {ok|not_found|deleted|kms_error|object_error|decrypt_error|audit_failed|error}
	AccessDuration   *prometheus.HistogramVec // labels: tenant_id, result

	// RetentionPassDuration measures one full retention sweep — pass label
	// is "cold_move" or "delete"; result is "ok" (sweep completed; per-row
	// errors are tracked separately on RetentionActionsTotal) or "error"
	// (the LIST query itself failed). Buckets cover 100ms .. ~17min.
	RetentionPassDuration *prometheus.HistogramVec

	// RetentionActionsTotal counts per-row outcomes inside a sweep. Bounded
	// cardinality on action ("cold_move" | "delete") and result
	// ("ok" | "stale" | "error" | "orphaned"). tenant_id is the high-card
	// dimension — keep alerts on action+result aggregates rather than
	// faceting per tenant in a global panel.
	//
	// Result semantics:
	//   - ok       — UPDATE matched, audit (and outbox, for delete) committed.
	//   - stale    — status-CAS rowsAffected=0 (concurrent flip; benign).
	//   - error    — Tx-scope error (DB write failed, or Phase A on delete).
	//   - orphaned — Phase A on delete reported ErrObjectNotFound; Phase B
	//                still proceeded so DB and S3 end up reconciled.
	RetentionActionsTotal *prometheus.CounterVec

	// IntegrityPassDuration measures one full integrity sweep. Result is
	// "ok" (sweep completed with at least one row processed; per-row
	// errors are tracked separately on IntegrityActionsTotal), "empty"
	// (SampleForVerify returned zero rows — distinct from "ok" so a hung
	// daemon doesn't look identical to a genuinely empty queue), or
	// "error" (the SampleForVerify query itself failed). Buckets cover
	// 100ms .. ~17min to track verify load against SLOs.
	IntegrityPassDuration *prometheus.HistogramVec

	// IntegrityActionsTotal counts per-row outcomes inside an integrity
	// sweep. Result is one of "ok" | "mismatch" | "error":
	//   - ok       — VerifyChecksum returned OK=true; verified_at +
	//                integrity_ok=true persisted, audit row written.
	//   - mismatch — VerifyChecksum returned OK=false (sha256 disagreement
	//                — corruption or tampering); verified_at +
	//                integrity_ok=false persisted, audit row written.
	//                Paired with an IntegrityFailuresTotal increment.
	//   - error    — VerifyChecksum returned an error OR the persistence
	//                Tx failed; verified_at NOT updated, no audit row,
	//                row stays eligible so the next sweep retries.
	IntegrityActionsTotal *prometheus.CounterVec

	// IntegrityFailuresTotal counts confirmed checksum mismatches per
	// tenant — the master spec §15.5 alerting metric. A non-zero rate
	// over the verify window indicates either real corruption (S3 object
	// drift, KMS issue) or active tampering. Distinct from
	// IntegrityActionsTotal{result=mismatch} so dashboards can keep a
	// dedicated alert pane on the failure metric without filtering
	// labels.
	IntegrityFailuresTotal *prometheus.CounterVec

	// LeaderActive is 1 on the replica currently holding the advisory
	// lock for a given pass and 0 elsewhere. Exactly one replica should
	// report 1 per pass at a time across the cluster — operators see
	// from metrics alone which replica is leading the sweeps. Mirrors
	// internal/dialer/retry/Metrics.LeaderActive but split by pass so
	// the retention and integrity slots are independent.
	//
	// Label "pass" values: "retention" | "integrity".
	LeaderActive *prometheus.GaugeVec
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

		AccessTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "access_total",
			Help:      "Number of OpenAudioStream calls broken out by result {ok|not_found|deleted|kms_error|object_error|decrypt_error|audit_failed|error}.",
		}, []string{"tenant_id", "result"}),

		AccessDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "access_duration_seconds",
			Help:      "Wall time of one OpenAudioStream call (lookup + KMS + S3 + decrypt + audit).",
			Buckets:   prometheus.DefBuckets,
		}, []string{"tenant_id", "result"}),

		RetentionPassDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "retention_pass_duration_seconds",
			Help:      "Wall time of one retention sweep (cold_move or delete pass).",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12), // 100ms .. ~17min
		}, []string{"pass", "result"}),

		RetentionActionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "retention_actions_total",
			Help:      "Per-row outcomes inside a retention sweep: action {cold_move|delete} × result {ok|stale|error|orphaned}.",
		}, []string{"tenant_id", "action", "result"}),

		IntegrityPassDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "integrity_pass_duration_seconds",
			Help:      "Wall time of one integrity sweep (sample + per-row verify + persist).",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12), // 100ms .. ~17min
		}, []string{"result"}),

		IntegrityActionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "integrity_actions_total",
			Help:      "Per-row outcomes inside an integrity sweep: result {ok|mismatch|error}.",
		}, []string{"tenant_id", "result"}),

		IntegrityFailuresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "integrity_failures_total",
			Help:      "Confirmed checksum mismatches per tenant (master spec §15.5).",
		}, []string{"tenant_id"}),

		LeaderActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "leader_active",
			Help:      "1 when this replica holds the lifecycle-pass advisory lock; 0 otherwise. Label pass={retention|integrity}.",
		}, []string{"pass"}),
	}

	if reg == nil {
		return m, nil
	}

	for _, c := range []prometheus.Collector{
		m.CommitTotal, m.StorageSizeBytes, m.CommitDuration,
		m.AccessTotal, m.AccessDuration,
		m.RetentionPassDuration, m.RetentionActionsTotal,
		m.IntegrityPassDuration, m.IntegrityActionsTotal, m.IntegrityFailuresTotal,
		m.LeaderActive,
	} {
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

// ObserveAccess ticks the access collectors. Safe to call on a nil receiver.
func (m *RecordingMetrics) ObserveAccess(tenantID, result string, durSec float64) {
	if m == nil {
		return
	}
	m.AccessTotal.WithLabelValues(tenantID, result).Inc()
	m.AccessDuration.WithLabelValues(tenantID, result).Observe(durSec)
}

// ObserveRetentionPass records one sweep's wall-clock duration. Safe on
// a nil receiver. pass is "cold_move" | "delete"; result is "ok" |
// "error".
func (m *RecordingMetrics) ObserveRetentionPass(pass, result string, durSec float64) {
	if m == nil {
		return
	}
	m.RetentionPassDuration.WithLabelValues(pass, result).Observe(durSec)
}

// IncRetentionAction increments the per-row outcome counter. Safe on a
// nil receiver. action is "cold_move" | "delete"; result is one of
// "ok" | "stale" | "error" | "orphaned" (orphaned only applies to delete).
func (m *RecordingMetrics) IncRetentionAction(tenantID, action, result string) {
	if m == nil {
		return
	}
	m.RetentionActionsTotal.WithLabelValues(tenantID, action, result).Inc()
}

// ObserveIntegrityPass records one integrity sweep's wall-clock
// duration. Safe on a nil receiver. result is "ok" | "empty" | "error":
// "empty" marks zero-row sweeps so a hung daemon doesn't look
// identical to a genuinely empty queue. The SampleForVerify query
// failure case is the only "error" outcome at the pass level (per-row
// VerifyChecksum errors are bucketed onto IntegrityActionsTotal
// instead).
func (m *RecordingMetrics) ObserveIntegrityPass(result string, durSec float64) {
	if m == nil {
		return
	}
	m.IntegrityPassDuration.WithLabelValues(result).Observe(durSec)
}

// IncIntegrityAction increments the per-row outcome counter. Safe on a
// nil receiver. result is one of "ok" | "mismatch" | "error".
func (m *RecordingMetrics) IncIntegrityAction(tenantID, result string) {
	if m == nil {
		return
	}
	m.IntegrityActionsTotal.WithLabelValues(tenantID, result).Inc()
}

// IncIntegrityFailure increments the confirmed-mismatch counter (master
// spec §15.5). Safe on a nil receiver. Callers MUST pair this with
// IncIntegrityAction(...,"mismatch") so the per-row dashboard and the
// dedicated failure-rate alert agree.
func (m *RecordingMetrics) IncIntegrityFailure(tenantID string) {
	if m == nil {
		return
	}
	m.IntegrityFailuresTotal.WithLabelValues(tenantID).Inc()
}

// SetLeaderActive writes the per-pass leader gauge. Safe on a nil
// receiver. pass is "retention" | "integrity"; active=true on a
// successful Acquire, false after Release / failed Acquire so peers'
// dashboards stay consistent.
func (m *RecordingMetrics) SetLeaderActive(pass string, active bool) {
	if m == nil {
		return
	}
	v := 0.0
	if active {
		v = 1.0
	}
	m.LeaderActive.WithLabelValues(pass).Set(v)
}

// Package metrics owns Prometheus collectors for the analytics module.
//
// Lifecycle: the composition root (cmd/api for the query path; cmd/worker
// for the ingest path) constructs a *prometheus.Registerer and passes it
// to RegisterIngestMetrics, which returns a populated *IngestMetrics.
// Per project carry-forward (recording module, dialer/retry): constructors
// return errors on duplicate registration — no init() / no MustRegister.
//
// Every helper is nil-safe: a nil *IngestMetrics receiver short-circuits,
// so unit tests can pass nil where a metric tick is irrelevant.
package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// IngestMetrics aggregates Prometheus collectors for the analytics
// ingest pipeline. A nil receiver is safe — every method becomes a no-op.
//
// Cardinality discipline: every CounterVec/HistogramVec carries at most
// the analytics-subject label (3 values: calls / operator_state /
// recording.uploaded) plus an optional bounded reason label on Failed
// (~6 enum values). The total per-Vec series count is bounded; we
// never label on tenant_id (high-cardinality) at this layer — the
// upstream realtime / recording dashboards do that, not the ingester.
type IngestMetrics struct {
	// Received counts every NATS message the ingester observed BEFORE
	// any dedup / decode / buffering decision. The unit is messages,
	// not rows. Useful for "is the bus delivering?" dashboards.
	Received *prometheus.CounterVec // labels: subject

	// Inserted counts ROWS that were successfully written to CH.
	// Incremented by the count of rows in a successful flush. A
	// disagreement between Received and Inserted (after subtracting
	// DedupHits / DeadLetter) signals lost data.
	Inserted *prometheus.CounterVec // labels: subject

	// Failed counts FLUSHES that failed (not rows). One failed flush of
	// 100 rows is one Failed increment — the row count is lost (those
	// rows are dropped per Plan 13.2 § Step 3.4 trade-off, dedup LRU
	// already absorbed their event_id).
	Failed *prometheus.CounterVec // labels: subject, reason

	// DeadLetter counts poison messages (malformed JSON, missing
	// event_id). The handler acks (returning nil) to prevent infinite
	// redelivery loops, and this counter is the only operational
	// signal that the producer is misbehaving.
	DeadLetter *prometheus.CounterVec // labels: subject

	// DedupHits counts messages whose event_id was already in the
	// per-subject LRU. The handler acks and skips the buffer.
	DedupHits *prometheus.CounterVec // labels: subject

	// BatchSize observes the row-count of every flush (including
	// ticker-driven flushes that may be small or zero). Buckets cover
	// 1..10_000 so the typical ~100-row flush lands on a bucket
	// boundary.
	BatchSize *prometheus.HistogramVec // labels: subject

	// FlushLatency observes the wall-clock duration of one flush call
	// (PrepareBatch + Append + Send for the relevant CH table). Slow
	// ClickHouse → growing 99p here.
	FlushLatency *prometheus.HistogramVec // labels: subject
}

// RegisterIngestMetrics constructs and (optionally) registers all
// collectors with reg. Returns the populated struct + a non-nil error
// if any registration fails. reg may be nil — collectors are still
// constructed but not registered (useful in unit tests that don't run
// a Prometheus server).
func RegisterIngestMetrics(reg prometheus.Registerer) (*IngestMetrics, error) {
	m := &IngestMetrics{
		Received: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_ingest",
			Name:      "received_total",
			Help:      "NATS messages observed by the analytics ingest pipeline, before dedup or decode.",
		}, []string{"subject"}),

		Inserted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_ingest",
			Name:      "inserted_total",
			Help:      "Rows successfully written to ClickHouse by the analytics ingest pipeline.",
		}, []string{"subject"}),

		Failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_ingest",
			Name:      "failed_total",
			Help:      "Flush attempts that failed (one increment per failed flush, not per lost row). Reason is one of {prepare_batch|send|other}.",
		}, []string{"subject", "reason"}),

		DeadLetter: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_ingest",
			Name:      "dead_letter_total",
			Help:      "Poison messages acked + dropped by the analytics ingest pipeline (malformed JSON, missing event_id, subject mismatch).",
		}, []string{"subject"}),

		DedupHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_ingest",
			Name:      "dedup_hits_total",
			Help:      "Messages whose event_id was already in the per-subject dedup LRU.",
		}, []string{"subject"}),

		BatchSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_ingest",
			Name:      "batch_size",
			Help:      "Row count of each flush call (ticker-driven or count-driven).",
			Buckets:   []float64{1, 10, 100, 1000, 10000},
		}, []string{"subject"}),

		FlushLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_ingest",
			Name:      "flush_latency_seconds",
			Help:      "Wall-clock duration of one analytics-ingest flush (PrepareBatch + Append + Send).",
			Buckets:   prometheus.DefBuckets,
		}, []string{"subject"}),
	}

	if reg == nil {
		return m, nil
	}

	for _, c := range []prometheus.Collector{
		m.Received, m.Inserted, m.Failed,
		m.DeadLetter, m.DedupHits,
		m.BatchSize, m.FlushLatency,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("analytics metrics: register: %w", err)
		}
	}
	return m, nil
}

// IncReceived ticks the per-subject received counter. nil-safe.
func (m *IngestMetrics) IncReceived(subject string) {
	if m == nil {
		return
	}
	m.Received.WithLabelValues(subject).Inc()
}

// IncInserted ticks the per-subject inserted counter by n rows. nil-safe.
// n is uint because every caller is a successful flush of len(buf).
func (m *IngestMetrics) IncInserted(subject string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.Inserted.WithLabelValues(subject).Add(float64(n))
}

// IncFailed ticks the per-(subject,reason) failure counter. reason is a
// bounded enum string: "prepare_batch" | "send" | "other". nil-safe.
func (m *IngestMetrics) IncFailed(subject, reason string) {
	if m == nil {
		return
	}
	m.Failed.WithLabelValues(subject, reason).Inc()
}

// IncDeadLetter ticks the per-subject dead-letter counter. nil-safe.
func (m *IngestMetrics) IncDeadLetter(subject string) {
	if m == nil {
		return
	}
	m.DeadLetter.WithLabelValues(subject).Inc()
}

// IncDedupHit ticks the per-subject dedup-hit counter. nil-safe.
func (m *IngestMetrics) IncDedupHit(subject string) {
	if m == nil {
		return
	}
	m.DedupHits.WithLabelValues(subject).Inc()
}

// ObserveBatchSize records a single batch's row count into the
// per-subject histogram. nil-safe; negative n is silently skipped.
func (m *IngestMetrics) ObserveBatchSize(subject string, n int) {
	if m == nil || n < 0 {
		return
	}
	m.BatchSize.WithLabelValues(subject).Observe(float64(n))
}

// ObserveFlushLatency records a single flush's wall-clock duration in
// seconds into the per-subject histogram. nil-safe.
func (m *IngestMetrics) ObserveFlushLatency(subject string, seconds float64) {
	if m == nil {
		return
	}
	m.FlushLatency.WithLabelValues(subject).Observe(seconds)
}

// QueryMetrics aggregates Prometheus collectors for the analytics
// read path (MetricsQuery + Redis cache). A nil receiver is safe —
// every method becomes a no-op so unit tests can pass nil where a
// metric tick is irrelevant.
//
// Cardinality discipline: every Vec carries at most the bounded
// "method" label (6 enum values: calls / operator_state /
// region_progress / hourly / operator_comparisons / overview). We
// never label on tenant_id (high-cardinality) at this layer — the
// upstream dashboards do that, not the query service.
type QueryMetrics struct {
	// QueryDuration observes the wall-clock duration of a single
	// MetricsQuery method end-to-end (cache lookup + CH query +
	// post-processing). Slow CH → growing 99p.
	QueryDuration *prometheus.HistogramVec // labels: method

	// CacheHits counts cache hits per method — successful read-through
	// shortcuts that avoided a CH query.
	CacheHits *prometheus.CounterVec // labels: method

	// CacheMisses counts cache misses per method — the query was
	// resolved against ClickHouse.
	CacheMisses *prometheus.CounterVec // labels: method
}

// RegisterQueryMetrics constructs and (optionally) registers all
// QueryMetrics collectors with reg. Returns the populated struct + a
// non-nil error if any registration fails. reg may be nil — collectors
// are still constructed but not registered (useful in unit tests).
func RegisterQueryMetrics(reg prometheus.Registerer) (*QueryMetrics, error) {
	m := &QueryMetrics{
		QueryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_query",
			Name:      "duration_seconds",
			Help:      "Wall-clock duration of a single analytics MetricsQuery method end-to-end.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method"}),

		CacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_query",
			Name:      "cache_hits_total",
			Help:      "Read-through cache hits per analytics MetricsQuery method.",
		}, []string{"method"}),

		CacheMisses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "analytics_query",
			Name:      "cache_misses_total",
			Help:      "Read-through cache misses per analytics MetricsQuery method (CH was hit).",
		}, []string{"method"}),
	}

	if reg == nil {
		return m, nil
	}

	for _, c := range []prometheus.Collector{m.QueryDuration, m.CacheHits, m.CacheMisses} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("analytics metrics: register query: %w", err)
		}
	}
	return m, nil
}

// IncCacheHit ticks the per-method cache-hit counter. nil-safe.
func (m *QueryMetrics) IncCacheHit(method string) {
	if m == nil {
		return
	}
	m.CacheHits.WithLabelValues(method).Inc()
}

// IncCacheMiss ticks the per-method cache-miss counter. nil-safe.
func (m *QueryMetrics) IncCacheMiss(method string) {
	if m == nil {
		return
	}
	m.CacheMisses.WithLabelValues(method).Inc()
}

// ObserveDuration records a single MetricsQuery method's wall-clock
// duration. nil-safe.
func (m *QueryMetrics) ObserveDuration(method string, seconds float64) {
	if m == nil {
		return
	}
	m.QueryDuration.WithLabelValues(method).Observe(seconds)
}

package api

import "context"

// IngestPipeline drains the ANALYTICS JetStream into ClickHouse.
// cmd/worker constructs one and calls Run; it blocks until ctx is cancelled.
type IngestPipeline interface {
	// Run blocks until ctx is cancelled. Idempotent on restart; redelivered
	// events are deduped via the LRU on event_id.
	Run(ctx context.Context) error
	// Stats returns runtime counters for /metrics.
	Stats() IngestStats
}

// MetricsQuery is the read surface used by the dashboard HTTP layer.
// All methods are bounded by Window; results are cached in Redis (30 s TTL).
type MetricsQuery interface {
	// Calls returns aggregated call counters and a status breakdown.
	Calls(ctx context.Context, q CallsQuery) (CallsResult, error)
	// OperatorState returns the time-in-state breakdown.
	OperatorState(ctx context.Context, q OperatorStateQuery) (OperatorStateBreakdown, error)
	// RegionProgress returns per-region completed/plan progress rows.
	RegionProgress(ctx context.Context, q RegionProgressQuery) ([]RegionProgressRow, error)
	// Hourly returns per-hour activity buckets within the window.
	Hourly(ctx context.Context, q HourlyQuery) ([]HourlyBucket, error)
	// OperatorComparisons returns one row per operator with relative metrics.
	OperatorComparisons(ctx context.Context, q OperatorComparisonsQuery) ([]OperatorComparisonRow, error)
}

// ServiceRO is the read-only aggregate used by the HTTP layer. It augments
// MetricsQuery with a single-call Overview.
type ServiceRO interface {
	MetricsQuery
	// Overview returns a four-section summary in one round trip.
	Overview(ctx context.Context, q OverviewQuery) (OverviewResult, error)
}

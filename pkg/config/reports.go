// Package config — reports block. Plan 13.3 Task 8 wires the runtime
// knobs consumed by:
//   - cmd/api's reports.Module.Register (AsyncThresholdPeriodDays,
//     AsyncThresholdRecords, PresignedURLTTL, QueueName).
//   - cmd/worker's reportsBoot asynq.Server (AsynqConcurrency, QueueName,
//     PresignedURLTTL).
//
// Plan 14 owns the artifact retention pass (JobTTL); we plumb the value
// here so the same `reports:` block in config.yaml serves every binary.
package config

import "time"

// ReportsConfig carries the reports-module runtime knobs.
type ReportsConfig struct {
	// AsyncThresholdPeriodDays is the half-open window length (in days)
	// above which the sync path refuses and returns ErrAsyncRequired,
	// forcing the caller through POST /api/reports/{kind}/export →
	// 202 + JobTicket. Default 30 (FR-I3).
	AsyncThresholdPeriodDays int `mapstructure:"async_threshold_period_days"`
	// AsyncThresholdRecords is the estimated-row threshold above which
	// the sync path refuses. Default 100_000 (FR-I3).
	AsyncThresholdRecords int `mapstructure:"async_threshold_records"`
	// JobTTL is the retention window for completed reports_jobs rows —
	// Plan 14 retention pass purges anything older. Default 720h (30d).
	JobTTL time.Duration `mapstructure:"job_ttl"`
	// PresignedURLTTL is the lifetime of the GET URL minted at
	// MarkSucceededTx. Default 24h per §FR-I3.
	PresignedURLTTL time.Duration `mapstructure:"presigned_url_ttl"`
	// AsynqConcurrency is the number of worker goroutines the reports
	// asynq.Server uses. Default 2; raise for higher throughput.
	AsynqConcurrency int `mapstructure:"asynq_concurrency"`
	// QueueName is the asynq queue the Consumer drains and the Queue
	// enqueues onto. Default "reports" (project per-module convention).
	QueueName string `mapstructure:"queue_name"`
}

// Validate applies sane defaults and rejects nothing — every field has
// a usable fallback. Wired into Config.Validate so a missing
// `reports:` block in config.yaml still produces a working config.
//
// The pointer receiver lets us mutate `c` in place; Config.Validate
// stores `c.Reports` by value, so subsequent reads see the populated
// defaults.
func (c *ReportsConfig) Validate() error {
	if c.AsyncThresholdPeriodDays <= 0 {
		c.AsyncThresholdPeriodDays = 30
	}
	if c.AsyncThresholdRecords <= 0 {
		c.AsyncThresholdRecords = 100_000
	}
	if c.JobTTL <= 0 {
		c.JobTTL = 720 * time.Hour // 30 days — Plan 14 retention default
	}
	if c.PresignedURLTTL <= 0 {
		c.PresignedURLTTL = 24 * time.Hour
	}
	if c.AsynqConcurrency <= 0 {
		c.AsynqConcurrency = 2
	}
	if c.QueueName == "" {
		c.QueueName = "reports"
	}
	return nil
}

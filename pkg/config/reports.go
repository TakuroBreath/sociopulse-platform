package config

import "time"

// ReportsConfig — Plan 14 owns the generators; we only plumb thresholds.
type ReportsConfig struct {
	AsyncThresholdPeriodDays int           `mapstructure:"async_threshold_period_days"`
	AsyncThresholdRecords    int           `mapstructure:"async_threshold_records"`
	JobTTL                   time.Duration `mapstructure:"job_ttl"`
	PresignedURLTTL          time.Duration `mapstructure:"presigned_url_ttl"`
}

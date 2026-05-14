// Package config — analytics block. Plan 13.2 § Task 6.
//
// AnalyticsConfig carries the runtime knobs consumed by:
//   - cmd/worker IngestPipeline (BatchSize, FlushInterval, DedupLRUSize,
//     QueueGroup, DrainTimeout).
//   - cmd/api analytics.Module read path (CacheShortTTL, CacheLongTTL,
//     LongWindowThreshold).
//
// The Enabled flag is the canonical "is analytics wired?" gate. When
// false, neither cmd/api's HTTP routes nor cmd/worker's ingest pipeline
// boot — both binaries log "analytics disabled" + INFO and skip the
// rest of the module's wiring. DSN-empty is a NESTED fallback: even
// when Enabled=true, an empty ClickHouse DSN still skips wiring with a
// WARN, so dev environments without a CH container keep booting.
//
// Layered onto DefaultDev() defaults in config.go; overridable via env
// vars SOCIOPULSE_ANALYTICS_* per the project's standard env-override
// convention (SOCIOPULSE_ + dotted key with `.` → `_`).
package config

import (
	"errors"
	"fmt"
	"time"
)

// AnalyticsConfig carries the analytics-module runtime knobs. See the
// package doc comment for the layered-config flow.
type AnalyticsConfig struct {
	// Enabled is the canonical "is analytics wired?" gate. False
	// disables BOTH the cmd/api HTTP query path and the cmd/worker
	// IngestPipeline.
	Enabled bool `mapstructure:"enabled"`

	// BatchSize is the per-subject row-count threshold the
	// IngestPipeline uses to trigger a flush. Required when Enabled.
	BatchSize int `mapstructure:"batch_size"`

	// FlushInterval is the wall-clock cadence at which the
	// IngestPipeline force-flushes every non-empty buffer regardless
	// of row count. Required when Enabled.
	FlushInterval time.Duration `mapstructure:"flush_interval"`

	// DedupLRUSize is the per-subject DedupLRU capacity. Required
	// when Enabled. Typical production value: 10_000.
	DedupLRUSize int `mapstructure:"dedup_lru_size"`

	// CacheShortTTL is the analytics-query Redis cache TTL for short
	// time-window queries (window < LongWindowThreshold). Required
	// when Enabled. Typical: 30s.
	CacheShortTTL time.Duration `mapstructure:"cache_short_ttl"`

	// CacheLongTTL is the analytics-query Redis cache TTL for long
	// time-window queries (window >= LongWindowThreshold). Required
	// when Enabled. Typical: 5m.
	CacheLongTTL time.Duration `mapstructure:"cache_long_ttl"`

	// LongWindowThreshold is the duration above which a query is
	// treated as a "long window" for cache-TTL purposes. Required
	// when Enabled. Typical: 24h.
	LongWindowThreshold time.Duration `mapstructure:"long_window_threshold"`

	// QueueGroup is the NATS push-consumer queue group used by the
	// IngestPipeline subscribers. Optional — the service layer
	// applies the "analytics-ingest" default when empty.
	QueueGroup string `mapstructure:"queue_group"`

	// DrainTimeout caps the time spent in the IngestPipeline's final
	// flush during ctx.Done drain. Optional — the service layer
	// applies the 5s default when zero.
	DrainTimeout time.Duration `mapstructure:"drain_timeout"`
}

// ErrInvalidAnalyticsConfig is the sentinel error returned by Validate.
// Callers errors.Is against this rather than message text.
var ErrInvalidAnalyticsConfig = errors.New("config: invalid analytics config")

// Validate enforces required-field presence when Enabled is true. When
// Enabled is false, validation is skipped entirely — a disabled module
// has no effective configuration.
//
// QueueGroup and DrainTimeout are optional; the service layer
// (analytics/service IngestConfig.applyDefaults) supplies their safe
// defaults at Run time.
func (c AnalyticsConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.BatchSize <= 0 {
		return fmt.Errorf("%w: batch_size must be > 0 (got %d)", ErrInvalidAnalyticsConfig, c.BatchSize)
	}
	if c.FlushInterval <= 0 {
		return fmt.Errorf("%w: flush_interval must be > 0 (got %s)", ErrInvalidAnalyticsConfig, c.FlushInterval)
	}
	if c.DedupLRUSize <= 0 {
		return fmt.Errorf("%w: dedup_lru_size must be > 0 (got %d)", ErrInvalidAnalyticsConfig, c.DedupLRUSize)
	}
	if c.CacheShortTTL <= 0 {
		return fmt.Errorf("%w: cache_short_ttl must be > 0 (got %s)", ErrInvalidAnalyticsConfig, c.CacheShortTTL)
	}
	if c.CacheLongTTL <= 0 {
		return fmt.Errorf("%w: cache_long_ttl must be > 0 (got %s)", ErrInvalidAnalyticsConfig, c.CacheLongTTL)
	}
	if c.LongWindowThreshold <= 0 {
		return fmt.Errorf("%w: long_window_threshold must be > 0 (got %s)", ErrInvalidAnalyticsConfig, c.LongWindowThreshold)
	}
	return nil
}

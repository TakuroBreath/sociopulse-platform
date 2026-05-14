package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAnalyticsConfig_Validate_DisabledSkipsAllChecks: when Enabled is
// false, every other field is permitted to be zero / nonsense. This
// matches the Plan 13.2 Task 6 § Q11 dual-target contract: cmd/api
// continues to boot when analytics is disabled, regardless of CH knobs.
func TestAnalyticsConfig_Validate_DisabledSkipsAllChecks(t *testing.T) {
	t.Parallel()
	cfg := AnalyticsConfig{Enabled: false}
	require.NoError(t, cfg.Validate())
}

// TestAnalyticsConfig_Validate_HappyPath: a sane production-shaped
// config validates cleanly.
func TestAnalyticsConfig_Validate_HappyPath(t *testing.T) {
	t.Parallel()
	cfg := AnalyticsConfig{
		Enabled:             true,
		BatchSize:           10_000,
		FlushInterval:       5 * time.Second,
		DedupLRUSize:        10_000,
		CacheShortTTL:       30 * time.Second,
		CacheLongTTL:        5 * time.Minute,
		LongWindowThreshold: 24 * time.Hour,
		QueueGroup:          "analytics-ingest",
		DrainTimeout:        5 * time.Second,
	}
	require.NoError(t, cfg.Validate())
}

func TestAnalyticsConfig_Validate_RejectsZeroBatchSize(t *testing.T) {
	t.Parallel()
	cfg := happyAnalyticsConfig()
	cfg.BatchSize = 0
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidAnalyticsConfig)
	require.Contains(t, err.Error(), "batch_size")
}

func TestAnalyticsConfig_Validate_RejectsNegativeBatchSize(t *testing.T) {
	t.Parallel()
	cfg := happyAnalyticsConfig()
	cfg.BatchSize = -1
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidAnalyticsConfig)
}

func TestAnalyticsConfig_Validate_RejectsZeroFlushInterval(t *testing.T) {
	t.Parallel()
	cfg := happyAnalyticsConfig()
	cfg.FlushInterval = 0
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidAnalyticsConfig)
	require.Contains(t, err.Error(), "flush_interval")
}

func TestAnalyticsConfig_Validate_RejectsZeroDedupLRUSize(t *testing.T) {
	t.Parallel()
	cfg := happyAnalyticsConfig()
	cfg.DedupLRUSize = 0
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidAnalyticsConfig)
	require.Contains(t, err.Error(), "dedup_lru_size")
}

func TestAnalyticsConfig_Validate_RejectsZeroCacheShortTTL(t *testing.T) {
	t.Parallel()
	cfg := happyAnalyticsConfig()
	cfg.CacheShortTTL = 0
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidAnalyticsConfig)
}

func TestAnalyticsConfig_Validate_RejectsZeroCacheLongTTL(t *testing.T) {
	t.Parallel()
	cfg := happyAnalyticsConfig()
	cfg.CacheLongTTL = 0
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidAnalyticsConfig)
}

func TestAnalyticsConfig_Validate_RejectsZeroLongWindowThreshold(t *testing.T) {
	t.Parallel()
	cfg := happyAnalyticsConfig()
	cfg.LongWindowThreshold = 0
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidAnalyticsConfig)
}

// TestAnalyticsConfig_Validate_OptionalsAllowedZero: QueueGroup and
// DrainTimeout are optional — IngestConfig.applyDefaults fills the
// sentinel values in the service layer. Validate MUST NOT reject them
// when zero / empty.
func TestAnalyticsConfig_Validate_OptionalsAllowedZero(t *testing.T) {
	t.Parallel()
	cfg := happyAnalyticsConfig()
	cfg.QueueGroup = ""
	cfg.DrainTimeout = 0
	require.NoError(t, cfg.Validate())
}

// happyAnalyticsConfig returns a known-good AnalyticsConfig used as a
// fixture for the per-field rejection cases. Tweak ONE field per test
// to exercise a single validation branch.
func happyAnalyticsConfig() AnalyticsConfig {
	return AnalyticsConfig{
		Enabled:             true,
		BatchSize:           10_000,
		FlushInterval:       5 * time.Second,
		DedupLRUSize:        10_000,
		CacheShortTTL:       30 * time.Second,
		CacheLongTTL:        5 * time.Minute,
		LongWindowThreshold: 24 * time.Hour,
	}
}

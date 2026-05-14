package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	reportsvc "github.com/sociopulse/platform/internal/reports/service"
)

// All test cases pin a fixed UTC instant so results are independent of the
// machine clock. The shared base lives on a package-level var so each
// subtest builds its Window from the same anchor.
var thresholdBase = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

func TestIsAsyncRequired_UnderThreshold(t *testing.T) {
	t.Parallel()

	w := analyticsapi.Window{From: thresholdBase, To: thresholdBase.Add(7 * 24 * time.Hour)}
	got := reportsvc.IsAsyncRequired(reportsvc.ThresholdConfig{
		AsyncPeriodDays:   30,
		AsyncRowThreshold: 100_000,
	}, w, 1_000, reportsapi.KindOperatorEfficiency)

	require.False(t, got, "7d window with 1k rows must take the sync path")
}

func TestIsAsyncRequired_OverPeriod(t *testing.T) {
	t.Parallel()

	w := analyticsapi.Window{From: thresholdBase, To: thresholdBase.Add(31 * 24 * time.Hour)}
	got := reportsvc.IsAsyncRequired(reportsvc.ThresholdConfig{
		AsyncPeriodDays:   30,
		AsyncRowThreshold: 100_000,
	}, w, 100, reportsapi.KindOperatorEfficiency)

	require.True(t, got, "31d window must force the async path")
}

func TestIsAsyncRequired_OverRows(t *testing.T) {
	t.Parallel()

	w := analyticsapi.Window{From: thresholdBase, To: thresholdBase.Add(7 * 24 * time.Hour)}
	got := reportsvc.IsAsyncRequired(reportsvc.ThresholdConfig{
		AsyncPeriodDays:   30,
		AsyncRowThreshold: 100_000,
	}, w, 100_000, reportsapi.KindCallsByStatus)

	require.True(t, got, "estRows >= threshold must force async")
}

func TestIsAsyncRequired_AlwaysForCustom(t *testing.T) {
	t.Parallel()

	w := analyticsapi.Window{From: thresholdBase, To: thresholdBase.Add(time.Hour)}
	got := reportsvc.IsAsyncRequired(reportsvc.ThresholdConfig{
		AsyncPeriodDays:   30,
		AsyncRowThreshold: 100_000,
	}, w, 1, reportsapi.KindCustom)

	require.True(t, got, "KindCustom must always take the async path")
}

func TestIsAsyncRequired_ZeroConfigFallsBackToDefaults(t *testing.T) {
	t.Parallel()

	// A window spanning 31 days trips the default 30-day fallback.
	wLong := analyticsapi.Window{From: thresholdBase, To: thresholdBase.Add(31 * 24 * time.Hour)}
	require.True(t, reportsvc.IsAsyncRequired(reportsvc.ThresholdConfig{}, wLong, 1, reportsapi.KindOperatorEfficiency),
		"zero-value config must apply the 30d period default")

	// estRows >= 100_000 trips the default row-threshold fallback.
	wShort := analyticsapi.Window{From: thresholdBase, To: thresholdBase.Add(time.Hour)}
	require.True(t, reportsvc.IsAsyncRequired(reportsvc.ThresholdConfig{}, wShort, 100_000, reportsapi.KindCallsByStatus),
		"zero-value config must apply the 100k row default")

	// Under both defaults — sync.
	require.False(t, reportsvc.IsAsyncRequired(reportsvc.ThresholdConfig{}, wShort, 1, reportsapi.KindOperatorEfficiency),
		"under-default zero-value config must stay sync")
}

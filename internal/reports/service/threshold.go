// Package service is the reports module's runtime service layer.
// Builds atop internal/reports/store for persistence, internal/analytics/api
// for read-side queries, internal/recording/storage for artifact upload,
// and pkg/outbox for atomic audit + report-ready event publishing.
package service

import (
	"time"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
)

// ThresholdConfig configures the sync-vs-async decision boundary.
// Loaded from pkg/config.ReportsConfig at construction time.
type ThresholdConfig struct {
	// AsyncPeriodDays is the window span (days) above which the request
	// must go async. Values <= 0 fall back to a 30 d default.
	AsyncPeriodDays int
	// AsyncRowThreshold is the estimated row count above which the
	// request must go async. Values <= 0 fall back to a 100_000 default.
	AsyncRowThreshold int
}

// Default ThresholdConfig values applied when a caller passes a zero
// value (the most common case in cmd/api wiring with a partial config).
const (
	defaultAsyncPeriodDays   = 30
	defaultAsyncRowThreshold = 100_000
)

// IsAsyncRequired returns true when the request MUST take the async path
// (Queue.Enqueue + asynq worker). The synchronous Runner.Run path refuses
// such requests with reportsapi.ErrAsyncRequired (Task 5 wires this).
//
// Decision rules (per Plan 13.3 §FR-I3 + combo-plan rule, line 209):
//  1. kind == KindCustom — always async (the user explicitly opted into
//     the async receipt by hitting POST /api/reports/custom).
//  2. estRows >= AsyncRowThreshold — heavy result set.
//  3. window span > AsyncPeriodDays — long historical query.
//
// AsyncPeriodDays/AsyncRowThreshold values <= 0 fall back to the
// defaults (30 days / 100_000 rows) so callers that pass a zero-value
// ThresholdConfig still get sensible behaviour.
func IsAsyncRequired(cfg ThresholdConfig, w analyticsapi.Window, estRows int, kind reportsapi.ReportKind) bool {
	if kind == reportsapi.KindCustom {
		return true
	}
	periodDays := cfg.AsyncPeriodDays
	if periodDays <= 0 {
		periodDays = defaultAsyncPeriodDays
	}
	rowThr := cfg.AsyncRowThreshold
	if rowThr <= 0 {
		rowThr = defaultAsyncRowThreshold
	}
	if estRows >= rowThr {
		return true
	}
	if w.To.Sub(w.From) > time.Duration(periodDays)*24*time.Hour {
		return true
	}
	return false
}

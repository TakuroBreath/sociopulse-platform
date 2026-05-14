// Package data carries the per-kind typed row shapes the Task 4
// renderers consume. The types live here (not under
// internal/reports/service) so the template packages and the service
// dispatcher can both import them without creating an import cycle
// (service ↔ templates).
//
// Fetchers — the analytics-shaping logic that produces these structs —
// continue to live under internal/reports/service/data.go; only the
// struct declarations moved.
package data

import (
	"github.com/google/uuid"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
)

// -----------------------------------------------------------------------
// OperatorEfficiency
// -----------------------------------------------------------------------

// OperatorEfficiencyData is the per-operator efficiency report payload
// the Task 4 XLSX/CSV/PDF renderers consume.
type OperatorEfficiencyData struct {
	Window analyticsapi.Window
	Rows   []OperatorEfficiencyRow
}

// OperatorEfficiencyRow is a single operator's metrics within the
// reporting window.
type OperatorEfficiencyRow struct {
	OperatorID   uuid.UUID
	DisplayName  string
	CallsTotal   uint64
	SuccessRate  float64
	AvgTalkSec   float64
	PauseShare   float64
	AboveTeamAvg bool
}

// -----------------------------------------------------------------------
// ProjectSummary
// -----------------------------------------------------------------------

// ProjectSummaryData aggregates the project-level Overview projection.
type ProjectSummaryData struct {
	Window  analyticsapi.Window
	Project uuid.UUID
	Calls   analyticsapi.CallsResult
	State   analyticsapi.OperatorStateBreakdown
	Regions []analyticsapi.RegionProgressRow
}

// -----------------------------------------------------------------------
// CallsByStatus
// -----------------------------------------------------------------------

// CallsByStatusData is the calls-by-status report payload.
type CallsByStatusData struct {
	Window analyticsapi.Window
	Result analyticsapi.CallsResult
}

// -----------------------------------------------------------------------
// Finance
// -----------------------------------------------------------------------

// FinanceData is the per-tenant finance report payload.
type FinanceData struct {
	Window        analyticsapi.Window
	Calls         analyticsapi.CallsResult
	PerMinuteRate float64 // ₽/min
	TotalMinutes  float64 // CallsResult.TotalDurSec / 60
	TotalCostRub  float64 // TotalMinutes * PerMinuteRate
}

// -----------------------------------------------------------------------
// QualityControl
// -----------------------------------------------------------------------

// QualityControlData is the quality-control report payload.
type QualityControlData struct {
	Window analyticsapi.Window
	Calls  analyticsapi.CallsResult
}

// -----------------------------------------------------------------------
// HourlyActivity
// -----------------------------------------------------------------------

// HourlyActivityData is the per-hour activity report payload.
type HourlyActivityData struct {
	Window  analyticsapi.Window
	Buckets []analyticsapi.HourlyBucket
}

// -----------------------------------------------------------------------
// Custom
// -----------------------------------------------------------------------

// CustomData is the custom report payload.
type CustomData struct {
	Window analyticsapi.Window
	OV     analyticsapi.OverviewResult
}

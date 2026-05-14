package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
)

// Per-kind data fetchers bridge analyticsapi.ServiceRO to typed per-kind
// row shapes the Task 4 renderers consume. Each fetcher:
//
//  1. decodes its Params slot (paramUUID / paramUUIDOpt / paramFloat),
//  2. assembles the analytics query,
//  3. calls the analytics service,
//  4. shapes the result into the kind-specific struct.
//
// Error policy: missing-or-unparseable Params surface
// reportsapi.ErrInvalidParams (wrapped via %w); analytics call errors
// surface as a wrap of the underlying error with a low-cardinality
// "<kind>: analytics" prefix.

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

// FetchOperatorEfficiency requires Params["project_id"]: string (uuid).
func FetchOperatorEfficiency(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (OperatorEfficiencyData, error) {
	projectID, err := paramUUID(in.Params, "project_id")
	if err != nil {
		return OperatorEfficiencyData{}, fmt.Errorf("operator_efficiency: %w", err)
	}
	rows, err := ana.OperatorComparisons(ctx, analyticsapi.OperatorComparisonsQuery{
		TenantID:  in.TenantID,
		ProjectID: projectID,
		Window:    in.Window,
	})
	if err != nil {
		return OperatorEfficiencyData{}, fmt.Errorf("operator_efficiency: analytics: %w", err)
	}
	out := OperatorEfficiencyData{Window: in.Window, Rows: make([]OperatorEfficiencyRow, 0, len(rows))}
	for _, r := range rows {
		out.Rows = append(out.Rows, OperatorEfficiencyRow{
			OperatorID:   r.OperatorID,
			DisplayName:  r.DisplayName,
			CallsTotal:   r.CallsTotal,
			SuccessRate:  r.SuccessRate,
			AvgTalkSec:   r.AvgTalkSec,
			PauseShare:   r.PauseShare,
			AboveTeamAvg: r.AboveTeamAvg,
		})
	}
	return out, nil
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

// FetchProjectSummary requires Params["project_id"]: string (uuid).
func FetchProjectSummary(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (ProjectSummaryData, error) {
	projectID, err := paramUUID(in.Params, "project_id")
	if err != nil {
		return ProjectSummaryData{}, fmt.Errorf("project_summary: %w", err)
	}
	ov, err := ana.Overview(ctx, analyticsapi.OverviewQuery{
		TenantID:  in.TenantID,
		ProjectID: &projectID,
		Window:    in.Window,
	})
	if err != nil {
		return ProjectSummaryData{}, fmt.Errorf("project_summary: analytics: %w", err)
	}
	return ProjectSummaryData{
		Window:  in.Window,
		Project: projectID,
		Calls:   ov.Calls,
		State:   ov.OperatorState,
		Regions: ov.RegionProgress,
	}, nil
}

// -----------------------------------------------------------------------
// CallsByStatus
// -----------------------------------------------------------------------

// CallsByStatusData is the calls-by-status report payload.
type CallsByStatusData struct {
	Window analyticsapi.Window
	Result analyticsapi.CallsResult
}

// FetchCallsByStatus optionally takes Params["project_id"]: string (uuid).
func FetchCallsByStatus(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (CallsByStatusData, error) {
	projectID, err := paramUUIDOpt(in.Params, "project_id")
	if err != nil {
		return CallsByStatusData{}, fmt.Errorf("calls_by_status: %w", err)
	}
	q := analyticsapi.CallsQuery{TenantID: in.TenantID, Window: in.Window}
	if projectID != uuid.Nil {
		q.ProjectID = &projectID
	}
	res, err := ana.Calls(ctx, q)
	if err != nil {
		return CallsByStatusData{}, fmt.Errorf("calls_by_status: analytics: %w", err)
	}
	return CallsByStatusData{Window: in.Window, Result: res}, nil
}

// -----------------------------------------------------------------------
// Finance
// -----------------------------------------------------------------------
//
// Finance is the "per-call cost" projection. Full margin/billing logic
// lives in Plan 14 (billing); for Plan 13.3 reports we surface
// CallsResult + a fixed per-minute rate supplied via
// Params["rate_rub_per_min"].

// FinanceData is the per-tenant finance report payload.
type FinanceData struct {
	Window        analyticsapi.Window
	Calls         analyticsapi.CallsResult
	PerMinuteRate float64 // ₽/min
	TotalMinutes  float64 // CallsResult.TotalDurSec / 60
	TotalCostRub  float64 // TotalMinutes * PerMinuteRate
}

// FetchFinance requires Params["rate_rub_per_min"]: float64 (or int).
// Optionally takes Params["project_id"]: string (uuid).
func FetchFinance(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (FinanceData, error) {
	rate, err := paramFloat(in.Params, "rate_rub_per_min")
	if err != nil {
		return FinanceData{}, fmt.Errorf("finance: %w", err)
	}
	if rate <= 0 {
		return FinanceData{}, fmt.Errorf("finance: %w: rate_rub_per_min must be positive", reportsapi.ErrInvalidParams)
	}
	projectID, err := paramUUIDOpt(in.Params, "project_id")
	if err != nil {
		return FinanceData{}, fmt.Errorf("finance: %w", err)
	}
	q := analyticsapi.CallsQuery{TenantID: in.TenantID, Window: in.Window}
	if projectID != uuid.Nil {
		q.ProjectID = &projectID
	}
	res, err := ana.Calls(ctx, q)
	if err != nil {
		return FinanceData{}, fmt.Errorf("finance: analytics: %w", err)
	}
	totalMin := float64(res.TotalDurSec) / 60.0
	return FinanceData{
		Window:        in.Window,
		Calls:         res,
		PerMinuteRate: rate,
		TotalMinutes:  totalMin,
		TotalCostRub:  totalMin * rate,
	}, nil
}

// -----------------------------------------------------------------------
// QualityControl
// -----------------------------------------------------------------------
//
// QualityControl is the calls-breakdown-with-quality-flags projection.
// v1 just surfaces CallsResult (refusals + fails are the signal); v2
// will add per-call review scores once those exist.

// QualityControlData is the quality-control report payload.
type QualityControlData struct {
	Window analyticsapi.Window
	Calls  analyticsapi.CallsResult
}

// FetchQualityControl optionally takes Params["project_id"]: string (uuid).
func FetchQualityControl(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (QualityControlData, error) {
	projectID, err := paramUUIDOpt(in.Params, "project_id")
	if err != nil {
		return QualityControlData{}, fmt.Errorf("quality_control: %w", err)
	}
	q := analyticsapi.CallsQuery{TenantID: in.TenantID, Window: in.Window}
	if projectID != uuid.Nil {
		q.ProjectID = &projectID
	}
	res, err := ana.Calls(ctx, q)
	if err != nil {
		return QualityControlData{}, fmt.Errorf("quality_control: analytics: %w", err)
	}
	return QualityControlData{Window: in.Window, Calls: res}, nil
}

// -----------------------------------------------------------------------
// HourlyActivity
// -----------------------------------------------------------------------

// HourlyActivityData is the per-hour activity report payload.
type HourlyActivityData struct {
	Window  analyticsapi.Window
	Buckets []analyticsapi.HourlyBucket
}

// FetchHourlyActivity optionally takes Params["project_id"]: string (uuid).
func FetchHourlyActivity(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (HourlyActivityData, error) {
	projectID, err := paramUUIDOpt(in.Params, "project_id")
	if err != nil {
		return HourlyActivityData{}, fmt.Errorf("hourly_activity: %w", err)
	}
	q := analyticsapi.HourlyQuery{TenantID: in.TenantID, Window: in.Window}
	if projectID != uuid.Nil {
		q.ProjectID = &projectID
	}
	buckets, err := ana.Hourly(ctx, q)
	if err != nil {
		return HourlyActivityData{}, fmt.Errorf("hourly_activity: analytics: %w", err)
	}
	return HourlyActivityData{Window: in.Window, Buckets: buckets}, nil
}

// -----------------------------------------------------------------------
// Custom
// -----------------------------------------------------------------------
//
// Custom is the free-form report. v1 just projects ServiceRO.Overview;
// future versions may parse a tiny DSL out of Params to compose
// multiple MetricsQuery calls.

// CustomData is the custom report payload.
type CustomData struct {
	Window analyticsapi.Window
	OV     analyticsapi.OverviewResult
}

// FetchCustom optionally takes Params["project_id"]: string (uuid).
func FetchCustom(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (CustomData, error) {
	projectID, err := paramUUIDOpt(in.Params, "project_id")
	if err != nil {
		return CustomData{}, fmt.Errorf("custom: %w", err)
	}
	q := analyticsapi.OverviewQuery{TenantID: in.TenantID, Window: in.Window}
	if projectID != uuid.Nil {
		q.ProjectID = &projectID
	}
	ov, err := ana.Overview(ctx, q)
	if err != nil {
		return CustomData{}, fmt.Errorf("custom: analytics: %w", err)
	}
	return CustomData{Window: in.Window, OV: ov}, nil
}

// -----------------------------------------------------------------------
// Pure param decoders
// -----------------------------------------------------------------------

// paramUUID returns the uuid at key. Missing or unparseable →
// ErrInvalidParams. Used for required UUID slots.
func paramUUID(p map[string]any, key string) (uuid.UUID, error) {
	raw, ok := p[key].(string)
	if !ok {
		return uuid.UUID{}, fmt.Errorf("%w: missing %s", reportsapi.ErrInvalidParams, key)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%w: %s: %w", reportsapi.ErrInvalidParams, key, err)
	}
	return id, nil
}

// paramUUIDOpt returns the uuid at key, or uuid.Nil if missing/empty.
// Returns an error only when the key is present, non-empty, and
// unparseable. The key argument is parameterised (rather than hard-coded
// to "project_id") so future fetchers with a different optional-UUID
// slot can reuse the helper without code duplication.
//
//nolint:unparam // key is intentionally parameterised for forward callers
func paramUUIDOpt(p map[string]any, key string) (uuid.UUID, error) {
	raw, ok := p[key].(string)
	if !ok || raw == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%s: parse uuid: %w", key, err)
	}
	return id, nil
}

// paramFloat returns the float64 at key. Accepts JSON-style untyped
// int / int64 / float64; nil or non-numeric → ErrInvalidParams.
func paramFloat(p map[string]any, key string) (float64, error) {
	switch v := p[key].(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case nil:
		return 0, fmt.Errorf("%w: missing %s", reportsapi.ErrInvalidParams, key)
	default:
		return 0, fmt.Errorf("%w: %s is not numeric (got %T)", reportsapi.ErrInvalidParams, key, v)
	}
}

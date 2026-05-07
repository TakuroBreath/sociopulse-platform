// Package api defines public contracts for the billing module.
// Other modules import only from this package — never from billing/service or billing/store.
//
// billing owns the per-tenant tariff store (telecom rates per trunk, operator
// wages per completed survey, storage rate, respondent-base purchase rate,
// fixed monthly fees), CostCalculator (pure function: call → cost in int64
// minor units), per-month spend breakdowns, per-project margin reports, and
// the finance dashboard. It subscribes to dialer.call.finalized and writes
// a call_costs row exactly-once (ON CONFLICT DO NOTHING on call_id).
//
// All money values are int64 minor units (Russian rouble kopecks): 100
// kopecks = 1 RUB. There is no float anywhere in the money path.
package api

import (
	"time"

	"github.com/google/uuid"
)

// Tariffs is the per-tenant tariff snapshot. Version monotonically increases
// on every update so consumers can detect staleness.
type Tariffs struct {
	TenantID             uuid.UUID
	Version              int
	UpdatedAt            time.Time
	TrunkCostsMinor      map[string]int64 // trunk_id → cost per minute (RUB minor)
	WagePerSurveyMinor   int64            // operator pay per completed survey
	StorageMinorPerGBMo  int64            // S3 storage rate per GB-month
	RespondentBasesMinor int64            // purchased respondent-base records, per-record
	FixedFeesMinor       int64            // monthly fixed fees
}

// Period is a half-open [From, To) time range used by month-grain reports.
type Period struct {
	From time.Time
	To   time.Time
}

// Month returns the [year, month] period in UTC, half-open over the
// calendar month. Useful for month-grain spend / margin queries.
func Month(year int, month time.Month) Period {
	from := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	return Period{From: from, To: to}
}

// CallCostInput is the input for CostCalculator.CallCost.
type CallCostInput struct {
	CallID       uuid.UUID
	TenantID     uuid.UUID
	ProjectID    uuid.UUID
	TrunkUsed    string
	DurationSec  int32
	Status       string
	StorageBytes int64
	FinalizedAt  time.Time
}

// CallCostOutput is the per-call cost decomposition. TotalMinor is the sum
// of the three components (storage cost is amortised per-call from the
// monthly storage rate by the implementation).
type CallCostOutput struct {
	TelecomMinor int64
	WagesMinor   int64
	StorageMinor int64
	TotalMinor   int64
}

// MonthBreakdown is one tenant×month spend row. The *Min fields are minor
// units; AvgCostPerMinuteMinor and CostPerSurveyMinor are convenience methods.
type MonthBreakdown struct {
	TenantID           uuid.UUID
	Period             Period
	TelecomMin         int64
	WagesMin           int64
	RespondentBasesMin int64
	StorageMin         int64
	FixedFeeMin        int64
	TotalMin           int64
	CompletedSurveys   int64
	TotalCallSeconds   int64
}

// CostPerSurveyMinor returns the average cost per completed survey in minor units.
// Returns zero when CompletedSurveys is zero.
func (b MonthBreakdown) CostPerSurveyMinor() int64 {
	if b.CompletedSurveys == 0 {
		return 0
	}
	return b.TotalMin / b.CompletedSurveys
}

// AvgCostPerMinuteMinor returns the average cost per minute of call time in
// minor units. Returns zero when TotalCallSeconds is zero.
func (b MonthBreakdown) AvgCostPerMinuteMinor() int64 {
	if b.TotalCallSeconds == 0 {
		return 0
	}
	// Cost per minute = total / (seconds / 60) = total * 60 / seconds.
	return b.TotalMin * 60 / b.TotalCallSeconds
}

// ProjectMargin is one row of the per-project margin report.
type ProjectMargin struct {
	ProjectID          uuid.UUID
	ProjectCode        string
	ProjectName        string
	Surveys            int64
	TelecomMin         int64
	WagesMin           int64
	RespondentBasesMin int64
	StorageMin         int64
	TotalMin           int64
	RevenueMin         int64
	MarginMin          int64
	CostPerSrvMn       int64
}

// DashboardResponse aggregates a tenant's current-period spend, per-project
// margins, and the trailing 12-month spend history for the finance UI.
type DashboardResponse struct {
	TenantID  uuid.UUID
	Period    Period
	Breakdown MonthBreakdown
	Projects  []ProjectMargin
	History   []MonthBreakdown // last 12 months
}

// TariffsResponse wraps a Tariffs for the HTTP layer.
type TariffsResponse struct {
	Tariffs Tariffs
}

// TariffsPatchRequest is the patch shape for the admin tariff editor.
// Pointer-typed scalars denote optional patches: nil means "leave unchanged".
// Map entries with a nil value delete the key.
type TariffsPatchRequest struct {
	TrunkCostsMinor      map[string]int64
	WagePerSurveyMinor   *int64
	StorageMinorPerGBMo  *int64
	RespondentBasesMinor *int64
	FixedFeesMinor       *int64
}

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
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Tariffs is the per-tenant tariff snapshot. Version monotonically increases
// on every update so consumers can detect staleness.
//
// All money values are int64 minor units (kopecks). The map TrunkCostsMinor
// is keyed by FreeSWITCH trunk_id (open enumeration — see
// docs/references/plan-14-billing.md §2.18). Unknown trunk IDs are
// defensively mapped to 0 by TrunkCostMinor so the calculator never
// panics on an event with a stale trunk reference.
type Tariffs struct {
	TenantID             uuid.UUID        `json:"tenant_id"`
	Version              int              `json:"version"`
	UpdatedAt            time.Time        `json:"updated_at"`
	TrunkCostsMinor      map[string]int64 `json:"trunk_costs_minor,omitempty" mapstructure:"trunk_costs_minor"`
	WagePerSurveyMinor   int64            `json:"wage_per_survey_minor"        mapstructure:"wage_per_survey_minor"`
	StorageMinorPerGBMo  int64            `json:"storage_minor_per_gb_mo"      mapstructure:"storage_minor_per_gb_mo"`
	RespondentBasesMinor int64            `json:"respondent_bases_minor"       mapstructure:"respondent_bases_minor"`
	FixedFeesMinor       int64            `json:"fixed_fees_minor"             mapstructure:"fixed_fees_minor"`
}

// Validate enforces non-negative invariants and non-empty trunk-ids.
// Returns nil for the zero value (a tenant with no tariffs at all is a
// legitimate state — TariffStore.Get returns ErrNoTariffs for that case
// before Validate is ever reached). All failure paths wrap ErrInvalidTariff
// so upstream callers can branch via errors.Is (canonical pkg/config
// pattern, mirrors pkg/config/analytics.go).
func (t Tariffs) Validate() error {
	if t.WagePerSurveyMinor < 0 {
		return fmt.Errorf("%w: wage_per_survey_minor < 0", ErrInvalidTariff)
	}
	if t.RespondentBasesMinor < 0 {
		return fmt.Errorf("%w: respondent_bases_minor < 0", ErrInvalidTariff)
	}
	if t.StorageMinorPerGBMo < 0 {
		return fmt.Errorf("%w: storage_minor_per_gb_mo < 0", ErrInvalidTariff)
	}
	if t.FixedFeesMinor < 0 {
		return fmt.Errorf("%w: fixed_fees_minor < 0", ErrInvalidTariff)
	}
	for trunkID, cost := range t.TrunkCostsMinor {
		if trunkID == "" {
			return fmt.Errorf("%w: empty trunk_id in trunk_costs_minor", ErrInvalidTariff)
		}
		if cost < 0 {
			return fmt.Errorf("%w: trunk_costs_minor[%q] < 0", ErrInvalidTariff, trunkID)
		}
	}
	return nil
}

// TrunkCostMinor returns the per-minute cost for a trunk, or 0 if the trunk
// is not configured (defensive — unknown trunk_used must NOT crash the
// calculator). Also returns 0 for an empty trunkID, which can occur when a
// call ends before the dialer selected a trunk (very short failure path).
func (t Tariffs) TrunkCostMinor(trunkID string) int64 {
	if trunkID == "" {
		return 0
	}
	return t.TrunkCostsMinor[trunkID]
}

// Period is a half-open [From, To) time range used by month-grain reports.
type Period struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
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
	CallID       uuid.UUID `json:"call_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	ProjectID    uuid.UUID `json:"project_id"`
	TrunkUsed    string    `json:"trunk_used"`
	DurationSec  int32     `json:"duration_sec"`
	Status       string    `json:"status"`
	StorageBytes int64     `json:"storage_bytes"`
	FinalizedAt  time.Time `json:"finalized_at"`
}

// CallCostOutput is the per-call cost decomposition. TotalMinor is the sum
// of the three components (storage cost is amortised per-call from the
// monthly storage rate by the implementation).
type CallCostOutput struct {
	TelecomMinor int64 `json:"telecom_minor"`
	WagesMinor   int64 `json:"wages_minor"`
	StorageMinor int64 `json:"storage_minor"`
	TotalMinor   int64 `json:"total_minor"`
}

// MonthBreakdown is one tenant×month spend row. The *Min fields are minor
// units; AvgCostPerMinuteMinor and CostPerSurveyMinor are convenience methods.
type MonthBreakdown struct {
	TenantID           uuid.UUID `json:"tenant_id"`
	Period             Period    `json:"period"`
	TelecomMin         int64     `json:"telecom_minor"`
	WagesMin           int64     `json:"wages_minor"`
	RespondentBasesMin int64     `json:"respondent_bases_minor"`
	StorageMin         int64     `json:"storage_minor"`
	FixedFeeMin        int64     `json:"fixed_fee_minor"`
	TotalMin           int64     `json:"total_minor"`
	CompletedSurveys   int64     `json:"completed_surveys"`
	TotalCallSeconds   int64     `json:"total_call_seconds"`
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
	ProjectID          uuid.UUID `json:"project_id"`
	ProjectCode        string    `json:"project_code"`
	ProjectName        string    `json:"project_name"`
	Surveys            int64     `json:"surveys"`
	TelecomMin         int64     `json:"telecom_minor"`
	WagesMin           int64     `json:"wages_minor"`
	RespondentBasesMin int64     `json:"respondent_bases_minor"`
	StorageMin         int64     `json:"storage_minor"`
	TotalMin           int64     `json:"total_minor"`
	RevenueMin         int64     `json:"revenue_minor"`
	MarginMin          int64     `json:"margin_minor"`
	CostPerSrvMn       int64     `json:"cost_per_survey_minor"`
}

// BreakdownItem is one slice of the dashboard pie chart (UI-ready shape).
// The service layer projects MonthBreakdown into a slice of BreakdownItems
// keyed by component label.
type BreakdownItem struct {
	Label    string `json:"label"`
	ValueMin int64  `json:"value_minor"`
}

// ByMonthItem is one bar of the dashboard byMonth chart (UI-ready shape).
// Label is a localised string the UI may display verbatim (e.g. "май 2026");
// Year + Month + ValueMin carry the structured data for re-sorting client side.
type ByMonthItem struct {
	Year     int    `json:"year"`
	Month    int    `json:"month"`
	Label    string `json:"label"`
	ValueMin int64  `json:"value_minor"`
}

// DashboardResponse is the explicit wire shape consumed by the finance UI
// (prototype admin-pages-2.jsx::AdminFinance). The internal MonthBreakdown
// remains the service-layer aggregate; this DTO is the HTTP projection.
type DashboardResponse struct {
	TenantID    uuid.UUID       `json:"tenant_id"`
	Period      Period          `json:"period"`
	MonthSpend  int64           `json:"month_spend_minor"`
	PrevSpend   int64           `json:"prev_spend_minor"`
	DeltaPct    float64         `json:"delta_pct"`
	CostPerSrv  int64           `json:"cost_per_survey_minor"`
	PrevCostSrv int64           `json:"prev_cost_per_survey_minor"`
	AvgCostMinM int64           `json:"avg_cost_per_minute_minor"`
	RevenueMin  int64           `json:"revenue_minor"`
	MarginMin   int64           `json:"margin_minor"`
	MarginPct   float64         `json:"margin_pct"`
	Breakdown   []BreakdownItem `json:"breakdown"`
	ByMonth     []ByMonthItem   `json:"by_month"`
	TopProjects []ProjectMargin `json:"top_projects"`
}

// TariffsResponse wraps a Tariffs for the HTTP layer. IsDefault signals
// that the returned snapshot is the BillingConfig.Defaults fallback (the
// tenant has not yet PATCHed its own tariffs).
type TariffsResponse struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	Tariffs   Tariffs   `json:"tariffs"`
	IsDefault bool      `json:"is_default"`
}

// TariffsPatchRequest is the patch shape for the admin tariff editor.
// Pointer-typed scalars denote optional patches: nil means "leave
// unchanged". TrunkCostsMinor is replace-all when present (the admin UI
// always submits the full map).
type TariffsPatchRequest struct {
	TrunkCostsMinor      map[string]int64 `json:"trunk_costs_minor,omitempty"`
	WagePerSurveyMinor   *int64           `json:"wage_per_survey_minor,omitempty"`
	StorageMinorPerGBMo  *int64           `json:"storage_minor_per_gb_mo,omitempty"`
	RespondentBasesMinor *int64           `json:"respondent_bases_minor,omitempty"`
	FixedFeesMinor       *int64           `json:"fixed_fees_minor,omitempty"`
}

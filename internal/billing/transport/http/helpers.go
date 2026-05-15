package http

import (
	"strconv"
	"time"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// previousSameLength returns the period immediately before p with the
// same duration. Used by Dashboard for the "vs previous" delta KPIs.
//
// Half-open arithmetic: prev.From = p.From − (p.To − p.From), prev.To =
// p.From. Two consecutive month periods give consecutive 30/31-day
// windows (whatever the upstream Month helper produced).
func previousSameLength(p billingapi.Period) billingapi.Period {
	d := p.To.Sub(p.From)
	return billingapi.Period{From: p.From.Add(-d), To: p.From}
}

// pctDelta returns 0 when prev is 0 (avoids inf), else (curr-prev)/prev * 100.
func pctDelta(curr, prev int64) float64 {
	if prev == 0 {
		return 0
	}
	return float64(curr-prev) / float64(prev) * 100
}

// marginPct returns 0 when revenue is 0, else (revenue-total)/revenue * 100.
// May be negative when the project is losing money — legitimate state.
func marginPct(revenue, total int64) float64 {
	if revenue == 0 {
		return 0
	}
	return float64(revenue-total) / float64(revenue) * 100
}

// topN returns the first n elements (or all if len <= n). Caller guarantees
// rows is already sorted (MarginReport.Margin returns highest-spend first).
func topN(rows []billingapi.ProjectMargin, n int) []billingapi.ProjectMargin {
	if len(rows) <= n {
		return rows
	}
	return rows[:n]
}

// buildBreakdown projects a MonthBreakdown into the UI pie-chart slice.
// Labels are in Russian (the platform's only locale in v1); the wire
// shape mirrors the AdminFinance prototype's pie-chart data feed.
func buildBreakdown(b billingapi.MonthBreakdown) []billingapi.BreakdownItem {
	return []billingapi.BreakdownItem{
		{Label: "Связь", ValueMin: b.TelecomMin},
		{Label: "Зарплата", ValueMin: b.WagesMin},
		{Label: "Базы", ValueMin: b.RespondentBasesMin},
		{Label: "Хранение", ValueMin: b.StorageMin},
		{Label: "Постоянные", ValueMin: b.FixedFeeMin},
	}
}

// toByMonthItems projects []MonthBreakdown into the UI bar-chart slice.
// Each item carries the structured Year+Month for client-side re-sorting
// and a localised Russian abbreviation Label for verbatim display.
func toByMonthItems(series []billingapi.MonthBreakdown) []billingapi.ByMonthItem {
	out := make([]billingapi.ByMonthItem, 0, len(series))
	for _, m := range series {
		out = append(out, billingapi.ByMonthItem{
			Year:     m.Period.From.Year(),
			Month:    int(m.Period.From.Month()),
			Label:    russianMonthShort(m.Period.From.Month()),
			ValueMin: m.TotalMin,
		})
	}
	return out
}

// russianMonthShort returns a 3-letter Russian month abbreviation. Index 0
// is unused so the slice indexes directly by time.Month (1..12).
func russianMonthShort(m time.Month) string {
	names := [...]string{"", "Янв", "Фев", "Мар", "Апр", "Май", "Июн",
		"Июл", "Авг", "Сен", "Окт", "Ноя", "Дек"}
	if m < 1 || int(m) >= len(names) {
		return ""
	}
	return names[m]
}

// parsePositiveInt parses s; returns (value, true) when within [minV, maxV]
// inclusive. Returns (0, false) on any parse error or out-of-range value
// — callers map that to the canonical 400 envelope.
func parsePositiveInt(s string, minV, maxV int) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < minV || n > maxV {
		return 0, false
	}
	return n, true
}

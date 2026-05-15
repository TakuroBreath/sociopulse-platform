package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
)

// MarginBackend is the narrow store interface MarginReport needs from the
// persistence layer. Production is *internal/billing/store/pgx.PG; unit
// tests substitute an in-memory fake. Per the project's "accept interfaces,
// return structs" convention, the interface lives in the consumer package.
type MarginBackend interface {
	// ListProjectsForPeriod returns one row per project that had non-zero
	// spend in [from, to). Empty projects are filtered at the SQL HAVING
	// clause; the result is sorted by project name.
	ListProjectsForPeriod(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]billingpgx.ProjectAggregate, error)
}

// marginReport composes the per-project margin report. Revenue is computed
// per project via the injected RevenueCalculator; cost columns come from
// ListProjectsForPeriod.
//
// Notable design decision: the bases-purchase line item is NOT included in
// per-project cost (RespondentBasesMin is intentionally left zero on every
// ProjectMargin row). Bases are billed at the TENANT grain — the same
// imported list may serve multiple projects — and prorating across projects
// would be arbitrary. The tenant-wide MonthBreakdown still captures bases
// in its dedicated RespondentBasesMin field.
type marginReport struct {
	pg  MarginBackend
	rev billingapi.RevenueCalculator
}

// NewMarginReport composes the per-project margin report. Revenue is
// computed per project via the injected RevenueCalculator; cost columns
// come from ListProjectsForPeriod. Rows are sorted by TotalMin descending
// so the dashboard's "top projects" widget gets the highest-spend projects
// first without an extra client-side sort.
func NewMarginReport(pg MarginBackend, rev billingapi.RevenueCalculator) billingapi.MarginReport {
	return &marginReport{pg: pg, rev: rev}
}

// Compile-time interface guard: any signature drift in billingapi.MarginReport
// breaks the build here rather than at the call site.
var _ billingapi.MarginReport = (*marginReport)(nil)

// Margin assembles per-project margin rows for the period. Implementation:
//
//  1. ListProjectsForPeriod — one row per project with non-zero spend.
//  2. For each row, compute revenue via the injected RevenueCalculator
//     (fee_per_completed × successful_calls).
//  3. Project the row into billingapi.ProjectMargin, derive Margin = Revenue −
//     Cost (can be negative — a project losing money is a legitimate
//     reported state).
//  4. Sort by TotalMin descending so the dashboard's top-projects widget
//     reads largest-first.
//
// Period.From must be non-zero and strictly less than Period.To, otherwise
// ErrInvalidPeriod is returned (mirrors the canonical validation rule in
// SpendReport.MonthSpend and RevenueCalculator.MonthRevenue).
func (m *marginReport) Margin(ctx context.Context, tenantID uuid.UUID, p billingapi.Period) ([]billingapi.ProjectMargin, error) {
	if p.From.IsZero() || !p.From.Before(p.To) {
		return nil, billingapi.ErrInvalidPeriod
	}
	rows, err := m.pg.ListProjectsForPeriod(ctx, tenantID, p.From, p.To)
	if err != nil {
		return nil, fmt.Errorf("billing/margin: list projects: %w", err)
	}
	out := make([]billingapi.ProjectMargin, 0, len(rows))
	for _, r := range rows {
		rev, err := m.rev.MonthRevenue(ctx, tenantID, r.ProjectID, p)
		if err != nil {
			return nil, fmt.Errorf("billing/margin: revenue for project %s: %w", r.ProjectID, err)
		}
		row := billingapi.ProjectMargin{
			ProjectID:   r.ProjectID,
			ProjectCode: r.ProjectCode,
			ProjectName: r.ProjectName,
			Surveys:     r.Surveys,
			TelecomMin:  r.TelecomMinor,
			WagesMin:    r.WagesMinor,
			// RespondentBasesMin intentionally left zero — bases are billed
			// at the tenant grain (same imported list may serve multiple
			// projects), so per-project proration would be arbitrary.
			// Tenant-wide bases are captured in MonthBreakdown.RespondentBasesMin.
			StorageMin: r.StorageMinor,
			TotalMin:   r.TotalMinor,
			RevenueMin: rev,
			MarginMin:  rev - r.TotalMinor,
		}
		if r.Surveys > 0 {
			// Integer division — sufficient for a UI display value. No
			// divide-by-zero guard needed below thanks to the Surveys > 0
			// branch.
			row.CostPerSrvMn = r.TotalMinor / r.Surveys
		}
		out = append(out, row)
	}
	// SliceStable preserves the SQL-side ORDER BY p.name secondary order
	// when two projects share TotalMin — so equal-spend rows in the
	// dashboard render in alphabetical order. Caught in Step F review.
	sort.SliceStable(out, func(i, j int) bool { return out[i].TotalMin > out[j].TotalMin })
	return out, nil
}

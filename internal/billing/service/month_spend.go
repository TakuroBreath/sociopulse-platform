package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
)

// AggregatorBackend is the narrow store interface SpendReport needs. The
// production implementation is *internal/billing/store/pgx.PG; unit tests
// substitute an in-memory fake. Lives in the consumer package per the
// "accept interfaces, return structs" convention also used by
// service.SettingsBackend and service.CostSink. Intra-module imports of
// store/pgx are permitted by the depguard module-boundaries rule (the
// deny list scopes CROSS-module access only).
type AggregatorBackend interface {
	// SumCallCosts aggregates call_costs in [from, to) for a tenant,
	// optionally scoped by projectID. nil projectID → tenant-wide.
	SumCallCosts(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, from, to time.Time) (billingpgx.CallCostsAggregate, error)
	// CountImportedRecords counts respondents.source='imported' in the
	// half-open period.
	CountImportedRecords(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, from, to time.Time) (int64, error)
}

// spendReport implements billingapi.SpendReport on top of an
// AggregatorBackend (call-cost sums) and a TariffStore (FixedFeesMinor +
// RespondentBasesMinor — line items that aren't stored per-call). The
// clock is injectable so SpendByMonth tests are deterministic.
//
// defTariffs is the BillingConfig.Defaults fallback used when a tenant
// has not yet configured its own tariffs (TariffStore.Get → ErrNoTariffs).
// The constructor reaches into the TariffStore via Get → ErrNoTariffs
// path and falls back to defTariffs so a fresh tenant still gets the
// platform's fixed monthly fee charged in its dashboard rather than zero.
type spendReport struct {
	pg         AggregatorBackend
	tariff     billingapi.TariffStore
	defTariffs billingapi.Tariffs
	now        func() time.Time
}

// NewSpendReport wires an aggregator backend, a TariffStore, and the
// platform defaults snapshot. Production uses time.Now as the clock; tests
// use NewSpendReportWithClock with a fixed Time.
//
// The TariffStore supplies FixedFeesMinor and RespondentBasesMinor —
// these two line items are NOT in call_costs (they're not per-call). The
// service-layer arithmetic multiplies CountImportedRecords by
// RespondentBasesMinor and adds FixedFeesMinor as a flat monthly charge.
// When the tenant has not yet configured tariffs, the defTariffs snapshot
// fills in — mirrors the NewCallFinalizedHandler defTariffs fallback so
// the two billing surfaces agree on what an un-configured tenant pays.
//
// Per docs/references/plan-14-billing.md §4.2 (money policy) we keep the
// arithmetic in int64 minor units — no float, no rounding here (the
// inputs are already integer-rounded by the calculator).
func NewSpendReport(pg AggregatorBackend, ts billingapi.TariffStore, defTariffs billingapi.Tariffs) billingapi.SpendReport {
	return &spendReport{pg: pg, tariff: ts, defTariffs: defTariffs, now: time.Now}
}

// NewSpendReportWithClock is the test-only constructor accepting an
// injected clock. A nil clock falls back to time.Now so callers can write
// `NewSpendReportWithClock(pg, ts, def, nil)` if they don't need
// determinism.
func NewSpendReportWithClock(pg AggregatorBackend, ts billingapi.TariffStore, defTariffs billingapi.Tariffs, now func() time.Time) billingapi.SpendReport {
	if now == nil {
		now = time.Now
	}
	return &spendReport{pg: pg, tariff: ts, defTariffs: defTariffs, now: now}
}

// Compile-time interface guard: any signature drift in billingapi.SpendReport
// breaks the build here rather than at the call site.
var _ billingapi.SpendReport = (*spendReport)(nil)

// MonthSpend assembles a MonthBreakdown for a single (tenant, optional
// project, period) tuple by:
//
//  1. SumCallCosts over the period — yields telecom/wages/storage/surveys/
//     duration from call_costs.
//  2. CountImportedRecords over the period — multiplied by
//     RespondentBasesMinor for the bases line item.
//  3. Tariffs.Get — supplies FixedFeesMinor (flat) and RespondentBasesMinor
//     (per-row). ErrNoTariffs is non-fatal: the TariffStore returns its
//     injected default snapshot for unset keys (see tariffStore.Get); the
//     zero-value case (no defaults configured) is still handled by treating
//     the error as a soft fallback.
//
// FixedFeeMin policy: charge the full FixedFeesMinor regardless of period
// length (week / month / quarter). The plan's abstract calls it a
// "fixed monthly fee"; the admin UI is month-grain by default, and weekly
// / quarterly views are aggregations OVER months. Prorating would invite
// double-counting bugs. If a future v2 wants strict proration, gate it
// behind an explicit BillingConfig flag.
func (r *spendReport) MonthSpend(
	ctx context.Context,
	tenantID uuid.UUID,
	projectID *uuid.UUID,
	p billingapi.Period,
) (billingapi.MonthBreakdown, error) {
	if p.From.IsZero() || !p.From.Before(p.To) {
		return billingapi.MonthBreakdown{}, billingapi.ErrInvalidPeriod
	}
	agg, err := r.pg.SumCallCosts(ctx, tenantID, projectID, p.From, p.To)
	if err != nil {
		return billingapi.MonthBreakdown{}, fmt.Errorf("billing/spend: sum call_costs: %w", err)
	}
	imported, err := r.pg.CountImportedRecords(ctx, tenantID, projectID, p.From, p.To)
	if err != nil {
		return billingapi.MonthBreakdown{}, fmt.Errorf("billing/spend: count imports: %w", err)
	}
	tariffs, err := r.tariff.Get(ctx, tenantID)
	switch {
	case errors.Is(err, billingapi.ErrNoTariffs):
		// Tenant has not configured tariffs — use the platform defaults.
		// Mirrors the NewCallFinalizedHandler ErrNoTariffs fallback so the
		// two billing surfaces (per-call ingestion and monthly rollup)
		// agree on what an un-configured tenant pays.
		tariffs = r.defTariffs
		tariffs.TenantID = tenantID
	case err != nil:
		return billingapi.MonthBreakdown{}, fmt.Errorf("billing/spend: load tariffs: %w", err)
	}

	bases := imported * tariffs.RespondentBasesMinor
	bd := billingapi.MonthBreakdown{
		TenantID:           tenantID,
		Period:             p,
		TelecomMin:         agg.TelecomMinor,
		WagesMin:           agg.WagesMinor,
		StorageMin:         agg.StorageMinor,
		RespondentBasesMin: bases,
		FixedFeeMin:        tariffs.FixedFeesMinor,
		CompletedSurveys:   agg.Surveys,
		TotalCallSeconds:   agg.TotalSeconds,
	}
	bd.TotalMin = bd.TelecomMin + bd.WagesMin + bd.StorageMin + bd.RespondentBasesMin + bd.FixedFeeMin
	return bd, nil
}

// SpendByMonth returns the trailing `count` months ending with the
// current month (oldest first). The interface doc-comment says
// "newest first" but the admin UI's by-month bar chart reads
// left-to-right oldest→newest; the canonical wire shape (DashboardResponse
// ByMonth) is also oldest-first. We honour the UI shape; if a future caller
// truly wants newest-first they can reverse on receipt.
//
// count must be in [1, 24] — 0/negative/>24 returns ErrInvalidPeriod. The
// upper bound prevents accidental 200-month queries from a buggy frontend.
func (r *spendReport) SpendByMonth(ctx context.Context, tenantID uuid.UUID, count int) ([]billingapi.MonthBreakdown, error) {
	if count <= 0 || count > 24 {
		return nil, billingapi.ErrInvalidPeriod
	}
	out := make([]billingapi.MonthBreakdown, 0, count)
	// Anchor to UTC so AddDate's month arithmetic is timezone-independent.
	now := r.now().UTC()
	for i := count - 1; i >= 0; i-- {
		m := now.AddDate(0, -i, 0)
		p := billingapi.Month(m.Year(), m.Month())
		bd, err := r.MonthSpend(ctx, tenantID, nil, p)
		if err != nil {
			return nil, err
		}
		out = append(out, bd)
	}
	return out, nil
}

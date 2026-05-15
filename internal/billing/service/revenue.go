package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// RevenueBackend is the narrow store interface RevenueCalculator needs. The
// production implementation is *internal/billing/store/pgx.PG; unit tests
// substitute an in-memory fake. The interface lives in the consumer
// package per the "accept interfaces, return structs" project convention.
type RevenueBackend interface {
	// ProjectFeePerCompleted returns projects.contract_fee_per_completed_minor
	// for a given (tenantID, projectID). Returns pgx.ErrNoRows when the
	// project is absent so the service can map it to zero revenue rather
	// than a hard error.
	ProjectFeePerCompleted(ctx context.Context, tenantID, projectID uuid.UUID) (int64, error)
	// CountSuccessfulCalls counts call_costs rows with status='success' in
	// the half-open period for a tenant×project.
	CountSuccessfulCalls(ctx context.Context, tenantID, projectID uuid.UUID, from, to time.Time) (int64, error)
}

// revenueCalc implements billingapi.RevenueCalculator. Revenue is derived
// from projects.contract_fee_per_completed_minor × count(status='success')
// in the period — there are no other revenue components in v1 (subscription
// fees, success bonuses, etc. are deferred).
type revenueCalc struct {
	pg RevenueBackend
}

// NewRevenueCalculator returns a billingapi.RevenueCalculator that derives
// revenue from projects.contract_fee_per_completed_minor × count of
// status='success' calls in the period.
//
// Returns 0 in two distinct scenarios — both legitimately "no revenue":
//  1. The project has no contract attached (fee == 0).
//  2. The project has been deleted (pgx.ErrNoRows from the fee lookup is
//     treated as missing-project → 0 revenue, NOT an error). This keeps
//     the margin report stable when a tenant archives a project mid-period.
func NewRevenueCalculator(pg RevenueBackend) billingapi.RevenueCalculator {
	return &revenueCalc{pg: pg}
}

// Compile-time interface guard: any signature drift in billingapi.RevenueCalculator
// breaks the build here rather than at the call site.
var _ billingapi.RevenueCalculator = (*revenueCalc)(nil)

// MonthRevenue returns total revenue in minor units for the period. The
// arithmetic is fee × successCount in int64 — no float, no rounding.
//
// Period.From must be non-zero and strictly less than Period.To, otherwise
// ErrInvalidPeriod is returned (mirrors the canonical validation rule in
// SpendReport.MonthSpend so the HTTP boundary's 400 mapping is consistent).
func (r *revenueCalc) MonthRevenue(ctx context.Context, tenantID, projectID uuid.UUID, p billingapi.Period) (int64, error) {
	if p.From.IsZero() || !p.From.Before(p.To) {
		return 0, billingapi.ErrInvalidPeriod
	}
	fee, err := r.pg.ProjectFeePerCompleted(ctx, tenantID, projectID)
	if errors.Is(err, pgxv5.ErrNoRows) {
		// Missing project (deleted/archived) — yield zero revenue rather
		// than failing the entire margin report.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("billing/revenue: load fee: %w", err)
	}
	if fee == 0 {
		// Project exists but no contract attached — zero revenue, skip the
		// COUNT query as a hot-path optimisation.
		return 0, nil
	}
	n, err := r.pg.CountSuccessfulCalls(ctx, tenantID, projectID, p.From, p.To)
	if err != nil {
		return 0, fmt.Errorf("billing/revenue: count success: %w", err)
	}
	return fee * n, nil
}

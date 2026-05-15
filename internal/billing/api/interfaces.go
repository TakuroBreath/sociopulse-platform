package api

import (
	"context"

	"github.com/google/uuid"
)

// CostCalculator is the pure-function call → cost surface. Implementations
// must be deterministic for a given (input, tariffs) pair so that re-runs
// produce the same number; tests rely on this.
type CostCalculator interface {
	// CallCost decomposes a finalised call into telecom, wages, storage,
	// and total cost in minor units.
	CallCost(ctx context.Context, in CallCostInput, t Tariffs) (CallCostOutput, error)
}

// TariffStore is the persistence surface for the per-tenant Tariffs.
type TariffStore interface {
	// Get returns the current tariffs for the tenant, or ErrNoTariffs.
	Get(ctx context.Context, tenantID uuid.UUID) (Tariffs, error)
	// Update merges the patch and writes a new version. The previous
	// version remains in the per-tenant tariff_history table for audit.
	Update(ctx context.Context, tenantID uuid.UUID, t Tariffs) (Tariffs, error)
}

// RevenueCalculator returns the platform revenue for a tenant×project×month.
// Implementations multiply CompletedSurveys by the per-tenant per-project
// price and add fixed fees.
type RevenueCalculator interface {
	// MonthRevenue returns total revenue in minor units for the period.
	MonthRevenue(ctx context.Context, tenantID, projectID uuid.UUID, p Period) (int64, error)
}

// MarginReport returns per-project margin rows for a period.
type MarginReport interface {
	// Margin returns one row per project with revenue, total cost, and margin.
	Margin(ctx context.Context, tenantID uuid.UUID, p Period) ([]ProjectMargin, error)
}

// SpendReport returns per-tenant (×optional project) monthly spend rollups.
type SpendReport interface {
	// MonthSpend returns the breakdown for a single month. projectID may be
	// nil for the tenant-wide total.
	MonthSpend(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, p Period) (MonthBreakdown, error)
	// SpendByMonth returns the trailing-`count`-months history (oldest first).
	// Ending month is the current calendar month in UTC; the slice is
	// chronologically ordered to mirror the dashboard ByMonth chart left-to-right.
	SpendByMonth(ctx context.Context, tenantID uuid.UUID, count int) ([]MonthBreakdown, error)
}

// CallFinalizedHook is the consumer side of dialer.call.finalized. It runs
// CostCalculator.CallCost and writes the call_costs row exactly-once
// (ON CONFLICT DO NOTHING on call_id).
type CallFinalizedHook interface {
	// OnCallFinalized is invoked once per dialer.call.finalized event.
	// Idempotent — safe to re-run if the NATS message is redelivered.
	OnCallFinalized(ctx context.Context, in CallCostInput) error
}

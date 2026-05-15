package pgx

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// CallCostsAggregate is one row returned by SumCallCosts. All fields are
// computed via SQL aggregate functions (SUM / COUNT FILTER) and coalesced
// to zero so callers never observe SQL NULL through the Scan boundary.
type CallCostsAggregate struct {
	TelecomMinor int64
	WagesMinor   int64
	StorageMinor int64
	Surveys      int64 // count(*) FILTER (where status='success')
	TotalSeconds int64 // sum(duration_sec)
}

// SumCallCosts aggregates call_costs rows finalised in the half-open
// period [from, to) for a tenant, optionally scoped by projectID. A nil
// projectID returns the tenant-wide rollup; a non-nil pointer adds an
// equality predicate on project_id (the call_costs_project_finalized
// index covers this path).
//
// Runs inside pool.WithTenant — RLS scopes the rows even though the WHERE
// clause is already tenant_id-pinned (belt-and-braces, and required for
// SET LOCAL app.tenant_id to apply). The Surveys count uses
// `count(*) FILTER (where status='success')` so the same query yields
// both the per-status survey count and the all-status telecom/wages/
// storage sums in a single round-trip.
func (s *PG) SumCallCosts(
	ctx context.Context,
	tenantID uuid.UUID,
	projectID *uuid.UUID,
	from, to time.Time,
) (CallCostsAggregate, error) {
	const qAll = `
select coalesce(sum(telecom_minor), 0)::bigint,
       coalesce(sum(wages_minor),   0)::bigint,
       coalesce(sum(storage_minor), 0)::bigint,
       coalesce(count(*) filter (where status = 'success'), 0)::bigint,
       coalesce(sum(duration_sec),  0)::bigint
  from call_costs
 where tenant_id   = $1
   and finalized_at >= $2
   and finalized_at <  $3`
	const qProj = `
select coalesce(sum(telecom_minor), 0)::bigint,
       coalesce(sum(wages_minor),   0)::bigint,
       coalesce(sum(storage_minor), 0)::bigint,
       coalesce(count(*) filter (where status = 'success'), 0)::bigint,
       coalesce(sum(duration_sec),  0)::bigint
  from call_costs
 where tenant_id   = $1
   and project_id  = $2
   and finalized_at >= $3
   and finalized_at <  $4`

	var agg CallCostsAggregate
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if projectID == nil {
			return tx.QueryRow(ctx, qAll, tenantID, from, to).
				Scan(&agg.TelecomMinor, &agg.WagesMinor, &agg.StorageMinor, &agg.Surveys, &agg.TotalSeconds)
		}
		return tx.QueryRow(ctx, qProj, tenantID, *projectID, from, to).
			Scan(&agg.TelecomMinor, &agg.WagesMinor, &agg.StorageMinor, &agg.Surveys, &agg.TotalSeconds)
	})
	if err != nil {
		return CallCostsAggregate{}, fmt.Errorf("billing/store: sum call_costs: %w", err)
	}
	return agg, nil
}

// CountImportedRecords returns the count of respondents.source='imported'
// for a tenant (optionally scoped by projectID) in the half-open period
// [from, to). Used by the bases-cost component of MonthBreakdown.
//
// Counted by `created_at` (the import event time), NOT by when the row was
// ultimately dialled. The bases line item bills the operator for the
// import itself, regardless of whether the row was later contacted —
// this matches the plan's "purchased-records" pricing model.
func (s *PG) CountImportedRecords(
	ctx context.Context,
	tenantID uuid.UUID,
	projectID *uuid.UUID,
	from, to time.Time,
) (int64, error) {
	const qAll = `
select count(*) from respondents
 where tenant_id = $1
   and source     = 'imported'
   and created_at >= $2
   and created_at <  $3`
	const qProj = `
select count(*) from respondents
 where tenant_id  = $1
   and project_id = $2
   and source     = 'imported'
   and created_at >= $3
   and created_at <  $4`

	var n int64
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if projectID == nil {
			return tx.QueryRow(ctx, qAll, tenantID, from, to).Scan(&n)
		}
		return tx.QueryRow(ctx, qProj, tenantID, *projectID, from, to).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("billing/store: count imported respondents: %w", err)
	}
	return n, nil
}

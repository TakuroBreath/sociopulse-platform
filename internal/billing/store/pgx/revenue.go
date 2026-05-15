package pgx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/sociopulse/platform/pkg/postgres"
)

// ProjectAggregate is one per-project row returned by ListProjectsForPeriod.
// Lives in the store package (NOT service) so the service layer can import
// it without creating a back-import cycle — see
// docs/references/plan-14-billing.md §4.10. Mirrors the leaf-package
// arrangement Plan 13.3 used for internal/reports/templates/data.
//
// Only projects with non-zero spend in the period are returned: the SQL
// HAVING clause filters out projects that had no call_costs activity in
// [from, to). Per-money fields are int64 minor units (kopecks).
type ProjectAggregate struct {
	ProjectID    uuid.UUID
	ProjectCode  string
	ProjectName  string
	Surveys      int64 // count(*) FILTER (where status='success')
	TelecomMinor int64
	WagesMinor   int64
	StorageMinor int64
	TotalMinor   int64
}

// ProjectFeePerCompleted returns the contract fee per completed survey in
// minor units for a given project. Returns 0 (not an error) when the
// project exists but has no contract attached
// (projects.contract_fee_per_completed_minor defaults to 0). Returns
// pgx.ErrNoRows when the project does not exist — callers (RevenueCalculator)
// treat the missing-project case as zero revenue, NOT a hard error, because
// a soft-deleted project should not break the margin report for the rest of
// the tenant.
//
// Tenant scope is enforced by pool.WithTenant; RLS belt-and-braces denies a
// forged tenantID via the projects_tenant_isolation policy.
func (s *PG) ProjectFeePerCompleted(ctx context.Context, tenantID, projectID uuid.UUID) (int64, error) {
	const q = `select coalesce(contract_fee_per_completed_minor, 0)::bigint
                 from projects where tenant_id = $1 and id = $2`
	var v int64
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, q, tenantID, projectID).Scan(&v)
	})
	if err != nil {
		if errors.Is(err, pgxv5.ErrNoRows) {
			// Re-return the sentinel directly (unwrapped) so the caller's
			// errors.Is check works regardless of which layer wraps it.
			return 0, pgxv5.ErrNoRows
		}
		return 0, fmt.Errorf("billing/store: project fee: %w", err)
	}
	return v, nil
}

// CountSuccessfulCalls returns the count of call_costs rows with
// status='success' for a tenant×project in the half-open period
// [from, to). Used by RevenueCalculator to multiply against
// ProjectFeePerCompleted for the per-project monthly revenue.
//
// finalized_at is the canonical time the dialer cut the call's lifecycle
// — matches the column index call_costs_project_finalized
// (project_id, finalized_at desc) so this query is index-supported.
//
// The COUNT is coalesced to zero so the Scan target is never SQL NULL
// even when zero rows match (defensive — count(*) is already non-null
// in standard SQL, but coalesce keeps the contract obvious).
func (s *PG) CountSuccessfulCalls(ctx context.Context, tenantID, projectID uuid.UUID, from, to time.Time) (int64, error) {
	const q = `select coalesce(count(*),0)::bigint
                 from call_costs
                where tenant_id = $1 and project_id = $2 and status = 'success'
                  and finalized_at >= $3 and finalized_at < $4`
	var n int64
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, q, tenantID, projectID, from, to).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("billing/store: count successful calls: %w", err)
	}
	return n, nil
}

// ListProjectsForPeriod returns one ProjectAggregate per project that had
// non-zero spend in [from, to). Rows are sorted by project name for
// predictable test output; the service layer (MarginReport) re-sorts by
// TotalMin desc for the UI's "top projects" ordering.
//
// Uses a LEFT JOIN call_costs so projects without any calls in the period
// are filtered out by the HAVING SUM(total_minor) > 0 clause — that path
// keeps the result set tight when a tenant has many archived projects.
// The query is index-supported by call_costs_project_finalized
// (project_id, finalized_at desc).
//
// All money columns are coalesced to zero at the SQL boundary so the
// Scan targets never observe SQL NULL.
func (s *PG) ListProjectsForPeriod(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]ProjectAggregate, error) {
	const q = `
select p.id, p.code, p.name,
       coalesce(count(cc.*) filter (where cc.status='success'),0)::bigint as surveys,
       coalesce(sum(cc.telecom_minor),0)::bigint  as telecom,
       coalesce(sum(cc.wages_minor),0)::bigint    as wages,
       coalesce(sum(cc.storage_minor),0)::bigint  as storage,
       coalesce(sum(cc.total_minor),0)::bigint    as total
  from projects p
  left join call_costs cc
       on cc.project_id = p.id
      and cc.tenant_id  = p.tenant_id
      and cc.finalized_at >= $2 and cc.finalized_at < $3
 where p.tenant_id = $1
 group by p.id, p.code, p.name
having coalesce(sum(cc.total_minor), 0) > 0
 order by p.name`
	var out []ProjectAggregate
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		rows, qerr := tx.Query(ctx, q, tenantID, from, to)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var r ProjectAggregate
			if err := rows.Scan(&r.ProjectID, &r.ProjectCode, &r.ProjectName,
				&r.Surveys, &r.TelecomMinor, &r.WagesMinor, &r.StorageMinor, &r.TotalMinor); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("billing/store: list projects: %w", err)
	}
	return out, nil
}

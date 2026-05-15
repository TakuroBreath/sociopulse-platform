//go:build integration

package pgx_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TestPG_ProjectFeePerCompleted_WithContract seeds a project, attaches a
// contract fee via setContractFee, and verifies the helper reads it back.
func TestPG_ProjectFeePerCompleted_WithContract(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, _ := seedTenantProjectCall(t, pool)
	setContractFee(t, pool, tid, pid, 38100)

	fee, err := store.ProjectFeePerCompleted(t.Context(), tid, pid)
	require.NoError(t, err)
	require.Equal(t, int64(38100), fee)
}

// TestPG_ProjectFeePerCompleted_NoContract_ReturnsZero pins the
// "project exists, no contract attached" path: the migration default
// (0 kopecks) is returned without an error. RevenueCalculator maps this
// to zero revenue.
func TestPG_ProjectFeePerCompleted_NoContract_ReturnsZero(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, _ := seedTenantProjectCall(t, pool)

	fee, err := store.ProjectFeePerCompleted(t.Context(), tid, pid)
	require.NoError(t, err)
	require.Equal(t, int64(0), fee)
}

// TestPG_ProjectFeePerCompleted_NotFound verifies the missing-project
// branch: a non-existent projectID returns pgx.ErrNoRows so the service
// layer can map it to zero revenue rather than failing the whole report.
func TestPG_ProjectFeePerCompleted_NotFound(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid := seedBillingTenant(t, pool)

	_, err := store.ProjectFeePerCompleted(t.Context(), tid, uuid.New())
	require.ErrorIs(t, err, pgxv5.ErrNoRows)
}

// TestPG_CountSuccessfulCalls seeds 2 success + 1 refused calls in May and
// verifies the helper returns 2 (only success rows count toward revenue).
func TestPG_CountSuccessfulCalls(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, _ := seedTenantProjectCall(t, pool)

	// Seed 2 successes + 1 refused. The seed helper uses fresh call_ids
	// each invocation (the canonical pattern from spend_pg_test.go).
	base := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		_, err := store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
			CallID:       seedCall(t, pool, tid, pid),
			TenantID:     tid,
			ProjectID:    pid,
			DurationSec:  60,
			Status:       "success",
			TelecomMinor: 342,
			WagesMinor:   12000,
			TotalMinor:   12342,
			FinalizedAt:  base.AddDate(0, 0, i),
		})
		require.NoError(t, err)
	}
	_, err := store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:       seedCall(t, pool, tid, pid),
		TenantID:     tid,
		ProjectID:    pid,
		DurationSec:  30,
		Status:       "refused",
		TelecomMinor: 171,
		TotalMinor:   171,
		FinalizedAt:  base.AddDate(0, 0, 5),
	})
	require.NoError(t, err)

	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	n, err := store.CountSuccessfulCalls(t.Context(), tid, pid, may, may.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
}

// TestPG_CountSuccessfulCalls_EmptyPeriod_ReturnsZero verifies the
// coalesce-to-zero path: a period with zero matching rows returns 0
// rather than NULL through Scan.
func TestPG_CountSuccessfulCalls_EmptyPeriod_ReturnsZero(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, _ := seedTenantProjectCall(t, pool)

	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n, err := store.CountSuccessfulCalls(t.Context(), tid, pid, jan, jan.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
}

// TestPG_ListProjectsForPeriod_OnlyNonzero verifies the HAVING filter:
// project 1 has spend in May, project 2 has none. The aggregator must
// return exactly one row (project 1) — the empty project is dropped at
// the SQL level so the service doesn't have to filter in Go.
func TestPG_ListProjectsForPeriod_OnlyNonzero(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid1, _ := seedTenantProjectCall(t, pool)
	_ = seedExtraProject(t, pool, tid) // pid2 has zero spend — must be filtered

	// Project 1 has one success call in May.
	base := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	_, err := store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:       seedCall(t, pool, tid, pid1),
		TenantID:     tid,
		ProjectID:    pid1,
		DurationSec:  60,
		Status:       "success",
		TelecomMinor: 342,
		WagesMinor:   12000,
		TotalMinor:   12342,
		FinalizedAt:  base,
	})
	require.NoError(t, err)

	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	rows, err := store.ListProjectsForPeriod(t.Context(), tid, may, may.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Len(t, rows, 1, "HAVING SUM(total_minor) > 0 must filter empty projects")
	require.Equal(t, pid1, rows[0].ProjectID)
	require.Equal(t, int64(12342), rows[0].TotalMinor)
	require.Equal(t, int64(1), rows[0].Surveys)
	require.Equal(t, int64(342), rows[0].TelecomMinor)
	require.Equal(t, int64(12000), rows[0].WagesMinor)
}

// TestPG_ListProjectsForPeriod_MultipleProjects_SortedByName verifies the
// ORDER BY p.name clause produces a stable, name-sorted output (the
// service layer re-sorts by TotalMin desc for the UI; the SQL output is
// deterministic for test convenience).
func TestPG_ListProjectsForPeriod_MultipleProjects_SortedByName(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pidA, _ := seedTenantProjectCall(t, pool)
	pidB := seedExtraProject(t, pool, tid)

	base := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	_, err := store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:      seedCall(t, pool, tid, pidA),
		TenantID:    tid,
		ProjectID:   pidA,
		DurationSec: 60,
		Status:      "success",
		TotalMinor:  10000,
		FinalizedAt: base,
	})
	require.NoError(t, err)
	_, err = store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:      seedCall(t, pool, tid, pidB),
		TenantID:    tid,
		ProjectID:   pidB,
		DurationSec: 60,
		Status:      "success",
		TotalMinor:  20000,
		FinalizedAt: base,
	})
	require.NoError(t, err)

	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	rows, err := store.ListProjectsForPeriod(t.Context(), tid, may, may.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// Project A is seeded with name "Test Project" (seedTenantProjectCall),
	// project B with "Test Project B" (seedExtraProject) — alphabetic order
	// is A then B regardless of TotalMinor.
	require.Equal(t, pidA, rows[0].ProjectID)
	require.Equal(t, pidB, rows[1].ProjectID)
}

// TestPG_ListProjectsForPeriod_Empty verifies the no-spend path: a tenant
// with projects but zero call_costs returns an empty slice (not nil).
func TestPG_ListProjectsForPeriod_Empty(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, _, _ := seedTenantProjectCall(t, pool)

	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	rows, err := store.ListProjectsForPeriod(t.Context(), tid, may, may.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Empty(t, rows)
}

// ─── Fixtures ────────────────────────────────────────────────────────────────

// setContractFee updates the project's contract_fee_per_completed_minor
// column. Test fixture only — uses pool.WithTenant because the
// projects_tenant_isolation policy lets the seeded tenant own its row.
func setContractFee(t *testing.T, pool *postgres.Pool, tid, pid uuid.UUID, fee int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tid, func(tx postgres.Tx) error {
		_, err := tx.Exec(context.Background(),
			`update projects set contract_fee_per_completed_minor = $1 where id = $2`,
			fee, pid)
		return err
	})
	require.NoError(t, err)
}

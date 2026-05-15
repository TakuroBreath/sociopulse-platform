//go:build integration

package pgx_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TestPG_SumCallCosts_TenantWide seeds three call_costs rows across two
// months and verifies the May tenant-wide aggregator returns the correct
// sums for telecom/wages plus a Surveys count filtered to status=success.
// One call is in April (outside the period) and is excluded.
func TestPG_SumCallCosts_TenantWide(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, callID1 := seedTenantProjectCall(t, pool)
	callID2 := seedCall(t, pool, tid, pid)
	callID3 := seedCall(t, pool, tid, pid)

	v := 1
	// Call 1: May, success, 60s, 342+12000.
	_, err := store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:        callID1,
		TenantID:      tid,
		ProjectID:     pid,
		TrunkUsed:     "mtt-msk-1",
		DurationSec:   60,
		Status:        "success",
		TelecomMinor:  342,
		WagesMinor:    12000,
		TotalMinor:    12342,
		TariffVersion: &v,
		FinalizedAt:   time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	// Call 2: May, refused, 30s, 171 (no wages on refused).
	_, err = store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:        callID2,
		TenantID:      tid,
		ProjectID:     pid,
		TrunkUsed:     "mtt-msk-1",
		DurationSec:   30,
		Status:        "refused",
		TelecomMinor:  171,
		TotalMinor:    171,
		TariffVersion: &v,
		FinalizedAt:   time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	// Call 3: April (outside period).
	_, err = store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:        callID3,
		TenantID:      tid,
		ProjectID:     pid,
		TrunkUsed:     "mtt-msk-1",
		DurationSec:   60,
		Status:        "success",
		TelecomMinor:  342,
		WagesMinor:    12000,
		TotalMinor:    12342,
		TariffVersion: &v,
		FinalizedAt:   time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	agg, err := store.SumCallCosts(t.Context(), tid, nil, may, may.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Equal(t, int64(342+171), agg.TelecomMinor) // both May rows
	require.Equal(t, int64(12000), agg.WagesMinor)     // only the success row has wages
	require.Equal(t, int64(0), agg.StorageMinor)
	require.Equal(t, int64(1), agg.Surveys)          // 1 success in May
	require.Equal(t, int64(60+30), agg.TotalSeconds) // sum of May durations
}

// TestPG_SumCallCosts_ProjectScoped verifies the project-scoped overload
// filters call_costs by project_id in addition to tenant_id.
func TestPG_SumCallCosts_ProjectScoped(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, callID := seedTenantProjectCall(t, pool)

	v := 1
	_, err := store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:        callID,
		TenantID:      tid,
		ProjectID:     pid,
		TrunkUsed:     "mtt-msk-1",
		DurationSec:   60,
		Status:        "success",
		TelecomMinor:  342,
		WagesMinor:    12000,
		TotalMinor:    12342,
		TariffVersion: &v,
		FinalizedAt:   time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	agg, err := store.SumCallCosts(t.Context(), tid, &pid, may, may.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Equal(t, int64(342), agg.TelecomMinor)
	require.Equal(t, int64(12000), agg.WagesMinor)
	require.Equal(t, int64(1), agg.Surveys)
	require.Equal(t, int64(60), agg.TotalSeconds)
}

// TestPG_SumCallCosts_EmptyPeriod_ReturnsZeros verifies the coalesce-to-zero
// path: a period with zero matching rows returns the all-zero aggregate
// rather than a SQL NULL surfaced via Scan.
func TestPG_SumCallCosts_EmptyPeriod_ReturnsZeros(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid := seedBillingTenant(t, pool)

	agg, err := store.SumCallCosts(t.Context(), tid, nil,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	require.Equal(t, billingpgx.CallCostsAggregate{}, agg)
}

// TestPG_CountImportedRecords seeds four respondents (3 imported across two
// months, 1 rdd in May) and verifies the May aggregator returns 2 — the two
// May-imported rows. The April-imported row is outside the window, and the
// RDD row is not source='imported'.
func TestPG_CountImportedRecords(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, _ := seedTenantProjectCall(t, pool)

	seedRespondent(t, pool, tid, pid, "imported", time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC))
	seedRespondent(t, pool, tid, pid, "imported", time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC))
	seedRespondent(t, pool, tid, pid, "rdd", time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC))
	seedRespondent(t, pool, tid, pid, "imported", time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC))

	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	n, err := store.CountImportedRecords(t.Context(), tid, nil, may, may.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
}

// TestPG_CountImportedRecords_ProjectScoped verifies the project filter:
// seeding two imports under projectA and one under projectB, asking for
// projectA returns 2.
func TestPG_CountImportedRecords_ProjectScoped(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pidA, _ := seedTenantProjectCall(t, pool)
	pidB := seedExtraProject(t, pool, tid)

	seedRespondent(t, pool, tid, pidA, "imported", time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC))
	seedRespondent(t, pool, tid, pidA, "imported", time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC))
	seedRespondent(t, pool, tid, pidB, "imported", time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC))

	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	n, err := store.CountImportedRecords(t.Context(), tid, &pidA, may, may.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
}

// TestPG_CountImportedRecords_Empty verifies the zero-row case returns 0,
// not SQL NULL.
func TestPG_CountImportedRecords_Empty(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid := seedBillingTenant(t, pool)

	may := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	n, err := store.CountImportedRecords(t.Context(), tid, nil, may, may.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
}

// ─── Fixtures ────────────────────────────────────────────────────────────────

// seedCall inserts a fresh calls row under (tenant, project) and returns
// its new call_id — used to extend seedTenantProjectCall's single-call
// fixture with additional rows for multi-row aggregation tests.
func seedCall(t *testing.T, pool *postgres.Pool, tid, pid uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tid, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO calls (id, tenant_id, project_id, started_at, status)
			 VALUES ($1, $2, $3, now(), 'success')`,
			id, tid, pid,
		)
		return err
	}))
	return id
}

// seedExtraProject inserts a second project under an existing tenant. Used
// to verify the project_id filter on CountImportedRecords. The project
// code is the full UUID — UUIDv7 starts with a millisecond timestamp, so
// two projects seeded back-to-back share their first 8 hex chars and
// collide on (tenant_id, code) uniqueness if we use a shortened slice.
func seedExtraProject(t *testing.T, pool *postgres.Pool, tid uuid.UUID) uuid.UUID {
	t.Helper()
	pid := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tid, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status)
			 VALUES ($1, $2, $3, 'Test Project B', 'active')`,
			pid, tid, "proj-"+pid.String(),
		)
		return err
	}))
	return pid
}

// seedRespondent inserts a minimal respondent row with the given source +
// created_at — the only columns the CountImportedRecords aggregator cares
// about, plus the NOT NULL columns (phone_encrypted/phone_hash/region_code)
// stuffed with id-derived dummy bytes. Uses WithTenant so RLS is
// satisfied. The phone_hash is keyed off the fresh row id so the
// respondents_tenant_project_phone_hash_uniq constraint
// (migration 000006) does not fire on repeated seeds for the same
// (tenant, project).
func seedRespondent(
	t *testing.T,
	pool *postgres.Pool,
	tid, pid uuid.UUID,
	source string,
	createdAt time.Time,
) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	idBytes := id[:] // 16-byte uuid — fine for both phone_hash and phone_encrypted
	require.NoError(t, pool.WithTenant(context.Background(), tid, func(tx postgres.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO respondents
			 (id, tenant_id, project_id, phone_encrypted, phone_hash, region_code, source, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, tid, pid, idBytes, idBytes, "RU-MOW", source, createdAt,
		)
		return err
	}))
}

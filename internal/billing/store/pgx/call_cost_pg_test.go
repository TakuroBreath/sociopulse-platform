//go:build integration

package pgx_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	billingpgx "github.com/sociopulse/platform/internal/billing/store/pgx"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TestPG_InsertCallCost_NewRow_Returnstrue verifies the fresh-INSERT path:
// a previously-unseen call_id lands as a row in call_costs, and the
// helper reports inserted=true.
func TestPG_InsertCallCost_NewRow_ReturnsTrue(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, callID := seedTenantProjectCall(t, pool)

	v := 1
	inserted, err := store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:        callID,
		TenantID:      tid,
		ProjectID:     pid,
		TrunkUsed:     "mtt-msk-1",
		DurationSec:   60,
		Status:        "success",
		TelecomMinor:  342,
		WagesMinor:    12000,
		StorageMinor:  0,
		TotalMinor:    12342,
		TariffVersion: &v,
		FinalizedAt:   time.Now().UTC().Truncate(time.Microsecond),
	})
	require.NoError(t, err)
	require.True(t, inserted, "fresh call_id must report inserted=true")

	// Cross-check: the row is visible under the tenant scope.
	var total int64
	require.NoError(t, pool.WithTenant(t.Context(), tid, func(tx postgres.Tx) error {
		return tx.QueryRow(t.Context(),
			`select total_minor from call_costs where call_id = $1`, callID,
		).Scan(&total)
	}))
	require.Equal(t, int64(12342), total)
}

// TestPG_InsertCallCost_IdempotentOnConflict verifies the at-least-once
// invariant from docs/references/plan-14-billing.md §4.3: a second
// InsertCallCost with the same call_id is a no-op and reports
// inserted=false. This is the canonical guard against NATS redelivery
// duplicating call_costs rows.
func TestPG_InsertCallCost_IdempotentOnConflict(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, callID := seedTenantProjectCall(t, pool)

	row := billingpgx.CallCostRow{
		CallID:      callID,
		TenantID:    tid,
		ProjectID:   pid,
		DurationSec: 60,
		Status:      "success",
		TotalMinor:  12342,
		FinalizedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	inserted, err := store.InsertCallCost(t.Context(), row)
	require.NoError(t, err)
	require.True(t, inserted)

	// Same call_id, different totals → DO NOTHING wins.
	row.TotalMinor = 99999
	inserted, err = store.InsertCallCost(t.Context(), row)
	require.NoError(t, err)
	require.False(t, inserted, "second insert must be idempotent skip")

	// The persisted row must still have the FIRST total — not overwritten.
	var total int64
	require.NoError(t, pool.WithTenant(t.Context(), tid, func(tx postgres.Tx) error {
		return tx.QueryRow(t.Context(),
			`select total_minor from call_costs where call_id = $1`, callID,
		).Scan(&total)
	}))
	require.Equal(t, int64(12342), total, "DO NOTHING must preserve the original row")
}

// TestPG_InsertCallCost_NullTariffVersion verifies the nullable column
// path — a hook running against the BillingConfig.Defaults fallback
// passes a nil *int and the column lands as SQL NULL.
func TestPG_InsertCallCost_NullTariffVersion(t *testing.T) {
	t.Parallel()
	pool := startBillingPGContainer(t)
	store := billingpgx.New(pool)
	tid, pid, callID := seedTenantProjectCall(t, pool)

	inserted, err := store.InsertCallCost(t.Context(), billingpgx.CallCostRow{
		CallID:        callID,
		TenantID:      tid,
		ProjectID:     pid,
		DurationSec:   60,
		Status:        "success",
		TotalMinor:    12342,
		TariffVersion: nil,
		FinalizedAt:   time.Now().UTC().Truncate(time.Microsecond),
	})
	require.NoError(t, err)
	require.True(t, inserted)

	var ver *int
	require.NoError(t, pool.WithTenant(t.Context(), tid, func(tx postgres.Tx) error {
		return tx.QueryRow(t.Context(),
			`select tariff_version from call_costs where call_id = $1`, callID,
		).Scan(&ver)
	}))
	require.Nil(t, ver, "nil pointer must land as SQL NULL")
}

// ─── Fixtures ────────────────────────────────────────────────────────────────

// seedTenantProjectCall extends the seedBillingTenant fixture: it
// inserts a tenant (via BypassRLS — tenants is admin-owned), then a
// project + a calls row under WithTenant. Returns all three IDs so
// call_costs FKs can be satisfied.
//
// Mirrors the canonical seedCall in
// internal/recording/store/postgres_pg_test.go (projects + calls under
// the tenant scope per the RLS isolation policy).
func seedTenantProjectCall(t *testing.T, pool *postgres.Pool) (tid, pid, callID uuid.UUID) {
	t.Helper()
	tid = seedBillingTenant(t, pool)
	pid = uuid.Must(uuid.NewV7())
	callID = uuid.Must(uuid.NewV7())

	require.NoError(t, pool.WithTenant(t.Context(), tid, func(tx postgres.Tx) error {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status)
			 VALUES ($1, $2, $3, 'Test Project', 'active')`,
			pid, tid, "proj-"+pid.String()[:8],
		); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(),
			`INSERT INTO calls (id, tenant_id, project_id, started_at, status)
			 VALUES ($1, $2, $3, now(), 'success')`,
			callID, tid, pid,
		)
		return err
	}))
	return tid, pid, callID
}

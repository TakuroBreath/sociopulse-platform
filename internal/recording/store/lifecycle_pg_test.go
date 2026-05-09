//go:build integration

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// ─────────────────────────────────────────────────────────────────────────────
// ListDueColdMoves
// ─────────────────────────────────────────────────────────────────────────────

// TestLifecycle_ListDueColdMoves_ReturnsOnlyDueStoredRows seeds three rows
// across two tenants:
//   - tenantA row1: status=stored, cold_at in the past (DUE)
//   - tenantA row2: status=stored, cold_at in the future (NOT due)
//   - tenantB row3: status=stored, cold_at in the past (DUE — different tenant)
//
// The cross-tenant sweep must return row1 + row3, never row2. Order: by
// cold_at ascending so the oldest backlog drains first.
func TestLifecycle_ListDueColdMoves_ReturnsOnlyDueStoredRows(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)

	// Two tenants — use seedTenantFull to avoid 8-char prefix collisions on
	// same-millisecond UUIDv7.
	tenantA := seedTenantFull(t, pool)
	tenantB := seedTenantFull(t, pool)

	// tenantA row 1 — due (cold_at 1h ago).
	callA1 := seedCallInTenant(t, pool, tenantA)
	rowA1 := newRow(t, tenantA, callA1)
	rowA1.ColdAt = now.Add(-1 * time.Hour)
	rowA1.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantA, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, rowA1)
		return err
	}))

	// tenantA row 2 — NOT due (cold_at +1h in future).
	callA2 := seedCallInTenant(t, pool, tenantA)
	rowA2 := newRow(t, tenantA, callA2)
	rowA2.ColdAt = now.Add(1 * time.Hour)
	rowA2.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantA, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, rowA2)
		return err
	}))

	// tenantB row 3 — due, different tenant.
	callB1 := seedCallInTenant(t, pool, tenantB)
	rowB1 := newRow(t, tenantB, callB1)
	rowB1.ColdAt = now.Add(-2 * time.Hour) // older than rowA1
	rowB1.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantB, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, rowB1)
		return err
	}))

	got, err := st.ListDueColdMoves(t.Context(), now, 100)
	require.NoError(t, err)
	require.Len(t, got, 2, "exactly the two due rows must come back")

	// Ordered by cold_at ASC: rowB1 (oldest) before rowA1.
	require.Equal(t, rowB1.ID, got[0].ID, "oldest cold_at must come first")
	require.Equal(t, rowA1.ID, got[1].ID, "younger due row second")

	// Every returned row carries enough cross-tenant data for the worker.
	for _, r := range got {
		require.NotEqual(t, uuid.Nil, r.TenantID)
		require.NotEqual(t, uuid.Nil, r.CallID)
		require.NotEmpty(t, r.S3Bucket)
		require.NotEmpty(t, r.AudioObjectKey)
		require.Equal(t, "stored", r.Status)
	}
}

// TestLifecycle_ListDueColdMoves_ExcludesNonStoredStatuses verifies that
// rows with status='cold' or status='deleted' are skipped — the cold-move
// transition only applies to status='stored'.
func TestLifecycle_ListDueColdMoves_ExcludesNonStoredStatuses(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := seedTenant(t, pool)

	for _, status := range []string{"cold", "deleted"} {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		row.Status = status
		row.ColdAt = now.Add(-1 * time.Hour)
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
	}

	got, err := st.ListDueColdMoves(t.Context(), now, 100)
	require.NoError(t, err)
	require.Empty(t, got, "non-stored rows must never appear in cold-move sweep")
}

// TestLifecycle_ListDueColdMoves_RespectsLimit seeds 5 due rows and asks
// for limit=3 — verifies the cap is honoured.
func TestLifecycle_ListDueColdMoves_RespectsLimit(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := seedTenant(t, pool)

	for i := range 5 {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		row.Status = "stored"
		row.ColdAt = now.Add(-time.Duration(i+1) * time.Hour)
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
	}

	got, err := st.ListDueColdMoves(t.Context(), now, 3)
	require.NoError(t, err)
	require.Len(t, got, 3, "limit must be honoured")
}

// rapi import is exercised below as soon as a test asserts on a
// LifecycleRow field; until then keep the symbol live so the file compiles.
var _ = rapi.LifecycleRow{}

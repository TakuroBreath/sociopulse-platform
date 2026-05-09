//go:build integration

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

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

// ─────────────────────────────────────────────────────────────────────────────
// ListDueDeletes
// ─────────────────────────────────────────────────────────────────────────────

// TestLifecycle_ListDueDeletes_FiltersStatusAndDueDate asserts the
// composite predicate: status IN ('stored','cold') AND delete_at IS NOT
// NULL AND delete_at <= now. We seed eight rows covering every relevant
// combination and assert exactly the expected three rows come back.
func TestLifecycle_ListDueDeletes_FiltersStatusAndDueDate(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := seedTenant(t, pool)

	past := now.Add(-1 * time.Hour)
	future := now.Add(1 * time.Hour)

	type spec struct {
		status   string
		deleteAt *time.Time
		due      bool // expected to come back
		label    string
	}
	specs := []spec{
		{"stored", &past, true, "stored+due"},
		{"cold", &past, true, "cold+due"},
		{"stored", &future, false, "stored+future"},
		{"cold", &future, false, "cold+future"},
		{"stored", nil, false, "stored+legalhold"},
		{"cold", nil, false, "cold+legalhold"},
		{"deleted", &past, false, "deleted+due"},
	}

	wantIDs := map[uuid.UUID]string{}
	for _, sp := range specs {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		row.Status = sp.status
		row.DeleteAt = sp.deleteAt
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
		if sp.due {
			wantIDs[row.ID] = sp.label
		}
	}

	got, err := st.ListDueDeletes(t.Context(), now, 100)
	require.NoError(t, err)
	require.Len(t, got, len(wantIDs), "exactly the due-and-not-legal-hold rows must come back")

	for _, r := range got {
		_, ok := wantIDs[r.ID]
		require.True(t, ok, "unexpected row %s in result (status=%s)", r.ID, r.Status)
		require.Contains(t, []string{"stored", "cold"}, r.Status)
		require.NotNil(t, r.DeleteAt, "DeleteAt must be non-nil for due-delete rows")
	}
}

// TestLifecycle_ListDueDeletes_OrdersByDeleteAtAscending seeds three due
// rows with staggered delete_at timestamps and verifies oldest-first order.
func TestLifecycle_ListDueDeletes_OrdersByDeleteAtAscending(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := seedTenant(t, pool)

	deleteTimestamps := []time.Time{
		now.Add(-3 * time.Hour),
		now.Add(-2 * time.Hour),
		now.Add(-1 * time.Hour),
	}
	wantOrder := make([]uuid.UUID, 0, 3)
	for _, ts := range deleteTimestamps {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		row.Status = "stored"
		t2 := ts // capture in own var
		row.DeleteAt = &t2
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
		wantOrder = append(wantOrder, row.ID)
	}

	got, err := st.ListDueDeletes(t.Context(), now, 10)
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i, want := range wantOrder {
		require.Equal(t, want, got[i].ID, "result %d must be the (i)-th oldest delete_at", i)
	}
}

// TestLifecycle_ListDueDeletes_RespectsLimit verifies the limit cap.
func TestLifecycle_ListDueDeletes_RespectsLimit(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := seedTenant(t, pool)

	for i := range 5 {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		row.Status = "stored"
		past := now.Add(-time.Duration(i+1) * time.Hour)
		row.DeleteAt = &past
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
	}

	got, err := st.ListDueDeletes(t.Context(), now, 2)
	require.NoError(t, err)
	require.Len(t, got, 2, "limit must be honoured")
}

// ─────────────────────────────────────────────────────────────────────────────
// MarkCold (status CAS: stored → cold)
// ─────────────────────────────────────────────────────────────────────────────

// TestLifecycle_MarkCold_HappyPath inserts a stored row and verifies that
// MarkCold transitions it to status='cold' and returns rowsAffected=1.
func TestLifecycle_MarkCold_HappyPath(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID, callID := seedCall(t, pool)
	row := newRow(t, tenantID, callID)
	row.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	n, err := st.MarkCold(t.Context(), row.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "happy MarkCold returns 1")

	got, err := st.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, "cold", got.Status, "status must be 'cold' after MarkCold")
}

// TestLifecycle_MarkCold_StaleReturnsZero verifies that calling MarkCold on
// a row whose status is already 'cold' (or anything other than 'stored')
// is a benign no-op: returns rowsAffected=0 with no error. Workers treat
// this as a concurrent-modify skip.
func TestLifecycle_MarkCold_StaleReturnsZero(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID, callID := seedCall(t, pool)
	row := newRow(t, tenantID, callID)
	row.Status = "cold" // already cold — pre-empt the CAS
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	n, err := st.MarkCold(t.Context(), row.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "stale MarkCold returns 0 rowsAffected")

	got, err := st.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, "cold", got.Status, "status must remain 'cold'")
}

// TestLifecycle_MarkCold_UnknownIDReturnsZero verifies that MarkCold with
// a non-existent ID is a benign no-op (rowsAffected=0, no error).
func TestLifecycle_MarkCold_UnknownIDReturnsZero(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	n, err := st.MarkCold(t.Context(), uuid.Must(uuid.NewV7()))
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
}

// ─────────────────────────────────────────────────────────────────────────────
// MarkDeleted (status CAS: stored|cold → deleted)
// ─────────────────────────────────────────────────────────────────────────────

// TestLifecycle_MarkDeleted_HappyPathFromStored covers the stored→deleted
// transition (skipping the cold step — the retention worker is allowed to
// jump straight to delete on a tenant with very short retention).
func TestLifecycle_MarkDeleted_HappyPathFromStored(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID, callID := seedCall(t, pool)
	row := newRow(t, tenantID, callID)
	row.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	n, err := st.MarkDeleted(t.Context(), row.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got, err := st.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, "deleted", got.Status)
}

// TestLifecycle_MarkDeleted_HappyPathFromCold covers the cold→deleted
// transition — the canonical retention path.
func TestLifecycle_MarkDeleted_HappyPathFromCold(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID, callID := seedCall(t, pool)
	row := newRow(t, tenantID, callID)
	row.Status = "cold"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	n, err := st.MarkDeleted(t.Context(), row.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got, err := st.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, "deleted", got.Status)
}

// TestLifecycle_MarkDeleted_StaleReturnsZero — calling MarkDeleted on an
// already-deleted row returns rowsAffected=0 (not an error).
func TestLifecycle_MarkDeleted_StaleReturnsZero(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID, callID := seedCall(t, pool)
	row := newRow(t, tenantID, callID)
	row.Status = "deleted"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	n, err := st.MarkDeleted(t.Context(), row.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
}

// ─────────────────────────────────────────────────────────────────────────────
// SampleForVerify
// ─────────────────────────────────────────────────────────────────────────────

// TestLifecycle_SampleForVerify_FullSampleExhaustsEligibleSet seeds five
// eligible rows (status='stored', verified_at NULL) and one ineligible row
// (status='deleted'). Asking for samplePct=100 must return all 5 eligible
// rows — the ineligible row is filtered out by the WHERE predicate.
func TestLifecycle_SampleForVerify_FullSampleExhaustsEligibleSet(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID := seedTenant(t, pool)

	eligibleIDs := map[uuid.UUID]bool{}
	for i := range 5 {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		// Half stored, half cold — both are eligible.
		if i%2 == 0 {
			row.Status = "stored"
		} else {
			row.Status = "cold"
		}
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
		eligibleIDs[row.ID] = true
	}

	// One deleted row — never eligible.
	deletedCall := seedCallInTenant(t, pool, tenantID)
	deletedRow := newRow(t, tenantID, deletedCall)
	deletedRow.Status = "deleted"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, deletedRow)
		return err
	}))

	got, err := st.SampleForVerify(t.Context(), 100.0, 100)
	require.NoError(t, err)
	require.Len(t, got, len(eligibleIDs), "100%% sample must return every eligible row")

	for _, r := range got {
		require.True(t, eligibleIDs[r.ID], "unexpected row %s in sample (status=%s)", r.ID, r.Status)
		require.Contains(t, []string{"stored", "cold"}, r.Status)
	}
}

// TestLifecycle_SampleForVerify_ExcludesRecentlyVerified seeds two rows:
// one with verified_at=now (NOT eligible) and one with verified_at=8 days
// ago (eligible). At samplePct=100 only the older one must come back.
func TestLifecycle_SampleForVerify_ExcludesRecentlyVerified(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID := seedTenant(t, pool)

	// Recent verify — must be skipped.
	recentCall := seedCallInTenant(t, pool, tenantID)
	recentRow := newRow(t, tenantID, recentCall)
	recentRow.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, recentRow)
		return err
	}))
	// Patch verified_at to "now" via BypassRLS — direct UPDATE.
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`UPDATE call_recordings SET verified_at = now(), integrity_ok = true WHERE id = $1`,
			recentRow.ID)
		return err
	}))

	// Old verify — eligible.
	oldCall := seedCallInTenant(t, pool, tenantID)
	oldRow := newRow(t, tenantID, oldCall)
	oldRow.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, oldRow)
		return err
	}))
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`UPDATE call_recordings SET verified_at = now() - interval '8 days', integrity_ok = true WHERE id = $1`,
			oldRow.ID)
		return err
	}))

	got, err := st.SampleForVerify(t.Context(), 100.0, 100)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the 8-day-stale verify row must come back")
	require.Equal(t, oldRow.ID, got[0].ID)
}

// TestLifecycle_SampleForVerify_RespectsLimit seeds 10 eligible rows and
// asks for limit=3 with samplePct=100 — verifies the LIMIT is honoured
// even at full sample.
func TestLifecycle_SampleForVerify_RespectsLimit(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID := seedTenant(t, pool)
	for range 10 {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		row.Status = "stored"
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
	}

	got, err := st.SampleForVerify(t.Context(), 100.0, 3)
	require.NoError(t, err)
	require.LessOrEqual(t, len(got), 3, "result must not exceed limit")
	require.NotEmpty(t, got, "100%% sample of 10 eligible rows must return at least one")
}

// ─────────────────────────────────────────────────────────────────────────────
// UpdateVerifyResult
// ─────────────────────────────────────────────────────────────────────────────

// TestLifecycle_UpdateVerifyResult_WritesVerifiedAtAndOK inserts a row
// without verified_at, calls UpdateVerifyResult(ok=true), and reads back
// to confirm verified_at is non-nil and integrity_ok=true.
func TestLifecycle_UpdateVerifyResult_WritesVerifiedAtAndOK(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID, callID := seedCall(t, pool)
	row := newRow(t, tenantID, callID)
	row.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	verifiedAt := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, st.UpdateVerifyResult(t.Context(), row.ID, verifiedAt, true))

	got, err := st.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.NotNil(t, got.VerifiedAt, "verified_at must be set after UpdateVerifyResult")
	require.True(t, got.VerifiedAt.Equal(verifiedAt),
		"verified_at must equal the caller-supplied timestamp (got %v, want %v)",
		got.VerifiedAt, verifiedAt)
	require.NotNil(t, got.IntegrityOK)
	require.True(t, *got.IntegrityOK)
}

// TestLifecycle_UpdateVerifyResult_RecordsFailure verifies that ok=false
// writes integrity_ok=false (the worker downstream will alert on this).
func TestLifecycle_UpdateVerifyResult_RecordsFailure(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID, callID := seedCall(t, pool)
	row := newRow(t, tenantID, callID)
	row.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	verifiedAt := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, st.UpdateVerifyResult(t.Context(), row.ID, verifiedAt, false))

	got, err := st.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.NotNil(t, got.IntegrityOK)
	require.False(t, *got.IntegrityOK)
}

// TestLifecycle_UpdateVerifyResult_Idempotent calls UpdateVerifyResult
// twice and verifies the second call is a benign overwrite (no error,
// final state matches the second call's values).
func TestLifecycle_UpdateVerifyResult_Idempotent(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID, callID := seedCall(t, pool)
	row := newRow(t, tenantID, callID)
	row.Status = "stored"
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	firstAt := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, st.UpdateVerifyResult(t.Context(), row.ID, firstAt, true))

	first, err := st.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.NotNil(t, first.VerifiedAt)
	require.NotNil(t, first.IntegrityOK)
	require.True(t, *first.IntegrityOK)

	// Second call with ok=false at a strictly-later timestamp — the row's
	// integrity flag flips and verified_at advances.
	secondAt := firstAt.Add(time.Second)
	require.NoError(t, st.UpdateVerifyResult(t.Context(), row.ID, secondAt, false))

	second, err := st.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.NotNil(t, second.VerifiedAt)
	require.NotNil(t, second.IntegrityOK)
	require.False(t, *second.IntegrityOK)
	require.False(t, second.VerifiedAt.Before(*first.VerifiedAt),
		"second verified_at must not regress")
}

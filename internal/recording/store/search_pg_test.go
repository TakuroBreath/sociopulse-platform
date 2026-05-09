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
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestSearch_RejectsBadLimit verifies that Limit=0 and Limit=201 both return
// errors without touching the database.
func TestSearch_RejectsBadLimit(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	st := store.NewPostgresStore(pool)

	_, err := st.Search(t.Context(), tenantID, store.SearchQ{Limit: 0})
	require.Error(t, err, "Limit=0 must return an error")

	_, err = st.Search(t.Context(), tenantID, store.SearchQ{Limit: 201})
	require.Error(t, err, "Limit=201 must return an error")
}

// TestSearch_RejectsHalfCursor verifies that supplying only one cursor field
// returns an error.
func TestSearch_RejectsHalfCursor(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	st := store.NewPostgresStore(pool)

	now := time.Now().UTC()
	_, err := st.Search(t.Context(), tenantID, store.SearchQ{
		Limit:             10,
		CursorCommittedAt: &now,
		// CursorRecordingID intentionally omitted
	})
	require.Error(t, err, "half cursor must return an error")
}

// TestSearch_FirstPageReturnsLatestFirst seeds 5 recordings in chronological
// order and verifies results come back in reverse-chronological order.
func TestSearch_FirstPageReturnsLatestFirst(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	st := store.NewPostgresStore(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)

	expectedIDs := make([]uuid.UUID, 0, 5)
	for i := range 5 {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		row.CommittedAt = now.Add(time.Duration(i) * time.Minute)
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
		expectedIDs = append(expectedIDs, row.ID)
	}

	got, err := st.Search(t.Context(), tenantID, store.SearchQ{Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 5)

	// DESC by committed_at — last-inserted first.
	for i, row := range got {
		require.Equal(t, expectedIDs[4-i], row.ID, "row %d should be the (4-i)-th seed", i)
	}
}

// TestSearch_KeysetPaginationWalksAllRecords seeds 5 recordings and walks
// through pages of size 2, verifying no overlap and no skip.
func TestSearch_KeysetPaginationWalksAllRecords(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	st := store.NewPostgresStore(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)

	allIDs := make([]uuid.UUID, 0, 5)
	for i := range 5 {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		row.CommittedAt = now.Add(time.Duration(i) * time.Minute)
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
		allIDs = append(allIDs, row.ID)
	}
	_ = allIDs // declared for intent clarity; verified via seen-set below

	// Page 1: 2 rows.
	got1, err := st.Search(t.Context(), tenantID, store.SearchQ{Limit: 2})
	require.NoError(t, err)
	require.Len(t, got1, 2)

	// Page 2: 2 rows using cursor from end of page 1.
	cursor1CA := got1[1].CommittedAt
	cursor1ID := got1[1].ID
	got2, err := st.Search(t.Context(), tenantID, store.SearchQ{
		Limit:             2,
		CursorCommittedAt: &cursor1CA,
		CursorRecordingID: &cursor1ID,
	})
	require.NoError(t, err)
	require.Len(t, got2, 2)

	// Page 3: 1 row.
	cursor2CA := got2[1].CommittedAt
	cursor2ID := got2[1].ID
	got3, err := st.Search(t.Context(), tenantID, store.SearchQ{
		Limit:             2,
		CursorCommittedAt: &cursor2CA,
		CursorRecordingID: &cursor2ID,
	})
	require.NoError(t, err)
	require.Len(t, got3, 1)

	// Page 4: empty.
	cursor3CA := got3[0].CommittedAt
	cursor3ID := got3[0].ID
	got4, err := st.Search(t.Context(), tenantID, store.SearchQ{
		Limit:             2,
		CursorCommittedAt: &cursor3CA,
		CursorRecordingID: &cursor3ID,
	})
	require.NoError(t, err)
	require.Empty(t, got4)

	// Verify no overlap between page 1 and page 2.
	p1IDs := map[uuid.UUID]bool{got1[0].ID: true, got1[1].ID: true}
	require.False(t, p1IDs[got2[0].ID], "page 2 row 0 must not appear on page 1")
	require.False(t, p1IDs[got2[1].ID], "page 2 row 1 must not appear on page 1")

	// Collect all IDs and verify we got all 5.
	seen := map[uuid.UUID]bool{}
	for _, r := range got1 {
		seen[r.ID] = true
	}
	for _, r := range got2 {
		seen[r.ID] = true
	}
	for _, r := range got3 {
		seen[r.ID] = true
	}
	require.Len(t, seen, 5, "pagination must cover all 5 rows without duplication")
}

// TestSearch_ProjectIDFilter seeds 3 recordings across 2 projects; filtering by
// projectA must return only the 2 rows for that project.
func TestSearch_ProjectIDFilter(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	st := store.NewPostgresStore(pool)

	projectA := uuid.Must(uuid.NewV7())
	projectB := uuid.Must(uuid.NewV7())
	seedProjectInTenant(t, pool, tenantID, projectA)
	seedProjectInTenant(t, pool, tenantID, projectB)

	// 2 calls under projectA, 1 under projectB.
	callA1 := seedCallWithProject(t, pool, tenantID, projectA)
	callA2 := seedCallWithProject(t, pool, tenantID, projectA)
	callB1 := seedCallWithProject(t, pool, tenantID, projectB)

	for _, callID := range []uuid.UUID{callA1, callA2, callB1} {
		row := newRow(t, tenantID, callID)
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
	}

	got, err := st.Search(t.Context(), tenantID, store.SearchQ{
		ProjectID: &projectA,
		Limit:     10,
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, r := range got {
		require.NotEqual(t, callB1, r.CallID, "result must not include projectB's call")
	}
}

// TestSearch_OperatorIDFilter seeds 3 recordings across 2 operators; filtering
// by operatorA must return only the 2 rows for that operator.
func TestSearch_OperatorIDFilter(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	st := store.NewPostgresStore(pool)

	projectID := uuid.Must(uuid.NewV7())
	seedProjectInTenant(t, pool, tenantID, projectID)

	operatorA := seedUserInTenant(t, pool, tenantID)
	operatorB := seedUserInTenant(t, pool, tenantID)

	// 2 calls under operatorA, 1 under operatorB.
	callA1 := seedCallWithOperator(t, pool, tenantID, projectID, &operatorA)
	callA2 := seedCallWithOperator(t, pool, tenantID, projectID, &operatorA)
	callB1 := seedCallWithOperator(t, pool, tenantID, projectID, &operatorB)

	for _, callID := range []uuid.UUID{callA1, callA2, callB1} {
		row := newRow(t, tenantID, callID)
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
	}

	got, err := st.Search(t.Context(), tenantID, store.SearchQ{
		OperatorID: &operatorA,
		Limit:      10,
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, r := range got {
		require.NotEqual(t, callB1, r.CallID, "result must not include operatorB's call")
	}
}

// TestSearch_StatusFilter seeds 3 recordings with statuses {stored, cold,
// deleted}; filtering by ["stored","cold"] must return only 2.
func TestSearch_StatusFilter(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	st := store.NewPostgresStore(pool)

	statuses := []string{"stored", "cold", "deleted"}
	for _, status := range statuses {
		callID := seedCallInTenant(t, pool, tenantID)
		row := newRow(t, tenantID, callID)
		row.Status = status
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
	}

	got, err := st.Search(t.Context(), tenantID, store.SearchQ{
		Status: []string{"stored", "cold"},
		Limit:  10,
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, r := range got {
		require.Contains(t, []string{"stored", "cold"}, r.Status)
	}
}

// TestSearch_PeriodFilter seeds 3 recordings spread 1 hour apart; filtering
// with From=t1, To=t2 returns only the middle recording (t1, inclusive) and
// excludes t2 (exclusive upper bound).
func TestSearch_PeriodFilter(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	st := store.NewPostgresStore(pool)

	base := time.Now().UTC().Truncate(time.Microsecond).Add(-3 * time.Hour)
	timestamps := []time.Time{
		base,
		base.Add(1 * time.Hour),
		base.Add(2 * time.Hour),
	}

	rows := make([]store.RecordingRow, 3)
	for i, ts := range timestamps {
		callID := seedCallInTenant(t, pool, tenantID)
		rows[i] = newRow(t, tenantID, callID)
		rows[i].CommittedAt = ts
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, rows[i])
			return err
		}))
	}

	// From=t1 inclusive, To=t2 exclusive: only the middle row (t1).
	from := timestamps[1]
	to := timestamps[2]
	got, err := st.Search(t.Context(), tenantID, store.SearchQ{
		From:  &from,
		To:    &to,
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, rows[1].ID, got[0].ID)
}

// TestSearch_ComboFilters verifies that ProjectID + Status + From/To compose
// correctly as AND conditions.
func TestSearch_ComboFilters(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	st := store.NewPostgresStore(pool)

	projectA := uuid.Must(uuid.NewV7())
	projectB := uuid.Must(uuid.NewV7())
	seedProjectInTenant(t, pool, tenantID, projectA)
	seedProjectInTenant(t, pool, tenantID, projectB)

	base := time.Now().UTC().Truncate(time.Microsecond)

	type seedSpec struct {
		projectID uuid.UUID
		status    string
		ts        time.Time
	}
	specs := []seedSpec{
		{projectA, "stored", base},
		{projectA, "cold", base.Add(1 * time.Hour)},
		{projectB, "stored", base.Add(2 * time.Hour)},
	}

	insertedRows := make([]store.RecordingRow, len(specs))
	for i, s := range specs {
		callID := seedCallWithProject(t, pool, tenantID, s.projectID)
		insertedRows[i] = newRow(t, tenantID, callID)
		insertedRows[i].Status = s.status
		insertedRows[i].CommittedAt = s.ts
		require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, insertedRows[i])
			return err
		}))
	}

	// Filter: projectA AND status=stored AND From=base AND To=base+30min.
	from := base
	to := base.Add(30 * time.Minute)
	got, err := st.Search(t.Context(), tenantID, store.SearchQ{
		ProjectID: &projectA,
		Status:    []string{"stored"},
		From:      &from,
		To:        &to,
		Limit:     10,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, insertedRows[0].ID, got[0].ID)
}

// TestSearch_TenantIsolation seeds two tenants × 3 recordings each and
// verifies that searching tenant A never leaks tenant B's rows.
func TestSearch_TenantIsolation(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantA := seedTenant(t, pool)
	tenantB := seedTenantFull(t, pool) // uses full UUID as org_code to avoid collision with tenantA
	st := store.NewPostgresStore(pool)

	for range 3 {
		callID := seedCallInTenant(t, pool, tenantA)
		row := newRow(t, tenantA, callID)
		require.NoError(t, pool.WithTenant(t.Context(), tenantA, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
	}

	bIDs := make([]uuid.UUID, 0, 3)
	for range 3 {
		callID := seedCallInTenant(t, pool, tenantB)
		row := newRow(t, tenantB, callID)
		require.NoError(t, pool.WithTenant(t.Context(), tenantB, func(tx postgres.Tx) error {
			_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
			return err
		}))
		bIDs = append(bIDs, row.ID)
	}

	got, err := st.Search(t.Context(), tenantA, store.SearchQ{Limit: 20})
	require.NoError(t, err)
	require.Len(t, got, 3, "tenant A must see exactly its 3 recordings")

	bIDSet := map[uuid.UUID]bool{}
	for _, id := range bIDs {
		bIDSet[id] = true
	}
	for _, r := range got {
		require.False(t, bIDSet[r.ID], "tenant A results must not contain tenant B recording %s", r.ID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Search-specific seed helpers (shared helpers live in postgres_pg_test.go).
// ─────────────────────────────────────────────────────────────────────────────

// seedCallInTenant seeds a project and call within an existing tenant.
func seedCallInTenant(t *testing.T, pool *postgres.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	projectID := uuid.Must(uuid.NewV7())
	callID := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status)
			 VALUES ($1, $2, $3, 'Test Project', 'active')`,
			projectID, tenantID, "proj-"+projectID.String(),
		); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(),
			`INSERT INTO calls (id, tenant_id, project_id, started_at, status)
			 VALUES ($1, $2, $3, now(), 'success')`,
			callID, tenantID, projectID,
		)
		return err
	}))
	return callID
}

// seedProjectInTenant inserts a project with the given ID into the tenant.
func seedProjectInTenant(t *testing.T, pool *postgres.Pool, tenantID, projectID uuid.UUID) {
	t.Helper()
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status)
			 VALUES ($1, $2, $3, 'Test Project', 'active')`,
			projectID, tenantID, "proj-"+projectID.String(),
		)
		return err
	}))
}

// seedCallWithProject inserts a call under an existing project in the tenant.
func seedCallWithProject(t *testing.T, pool *postgres.Pool, tenantID, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	callID := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO calls (id, tenant_id, project_id, started_at, status)
			 VALUES ($1, $2, $3, now(), 'success')`,
			callID, tenantID, projectID,
		)
		return err
	}))
	return callID
}

// seedUserInTenant inserts a user (operator) in the tenant and returns its ID.
// Uses the post-migration schema: roles text[] (no role/status columns after 000003).
func seedUserInTenant(t *testing.T, pool *postgres.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	userID := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO users (id, tenant_id, login, password_hash, full_name, roles)
			 VALUES ($1, $2, $3, 'hash', 'Test Operator', ARRAY['operator'])`,
			userID, tenantID, "op-"+userID.String(),
		)
		return err
	}))
	return userID
}

// seedCallWithOperator inserts a call under the given project and operator.
func seedCallWithOperator(t *testing.T, pool *postgres.Pool, tenantID, projectID uuid.UUID, operatorID *uuid.UUID) uuid.UUID {
	t.Helper()
	callID := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO calls (id, tenant_id, project_id, operator_id, started_at, status)
			 VALUES ($1, $2, $3, $4, now(), 'success')`,
			callID, tenantID, projectID, operatorID,
		)
		return err
	}))
	return callID
}

// seedTenantFull is like seedTenant but uses the full UUID string as org_code
// to avoid the short-prefix collision that can occur when two tenants are seeded
// within the same millisecond (UUIDv7 shares top bits across same-ms values).
func seedTenantFull(t *testing.T, pool *postgres.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			 VALUES ($1, $2, $3, 'active', 'kms-test', '\x00')`,
			id,
			"org-"+id.String(), // full UUID avoids same-millisecond collision
			"tenant-"+id.String(),
		)
		return err
	}))
	return id
}

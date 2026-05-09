//go:build integration

package service_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// seedCallInTenant inserts a project + call inside an existing tenant.
// Returns the new call_id.
func seedCallInTenant(t *testing.T, pool *postgres.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	projectID := uuid.Must(uuid.NewV7())
	callID := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status) VALUES ($1, $2, $3, 'Test Project', 'active')`,
			projectID, tenantID, "proj-"+projectID.String(),
		); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(),
			`INSERT INTO calls (id, tenant_id, project_id, started_at, status) VALUES ($1, $2, $3, now(), 'success')`,
			callID, tenantID, projectID,
		)
		return err
	}))
	return callID
}

// TestService_Search_FirstPage commits 3 recordings and asserts the first
// (and only) page returns all 3 with HasMore=false and an empty NextCursor.
func TestService_Search_FirstPage(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	svc := buildService(t, pool)

	for range 3 {
		callID := seedCallInTenant(t, pool, tenantID)
		_, err := svc.Commit(t.Context(), newCommitInput(t, tenantID, callID))
		require.NoError(t, err)
	}

	result, err := svc.Search(t.Context(), tenantID, rapi.SearchQuery{Limit: 10})
	require.NoError(t, err)
	require.Len(t, result.Items, 3)
	require.False(t, result.HasMore)
	require.Empty(t, result.NextCursor)
}

// TestService_Search_PaginatesViaNextCursor seeds 3 recordings, fetches page 1
// (Limit=2), then walks to page 2 via NextCursor and verifies no overlap.
func TestService_Search_PaginatesViaNextCursor(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	svc := buildService(t, pool)

	for range 3 {
		callID := seedCallInTenant(t, pool, tenantID)
		_, err := svc.Commit(t.Context(), newCommitInput(t, tenantID, callID))
		require.NoError(t, err)
	}

	result1, err := svc.Search(t.Context(), tenantID, rapi.SearchQuery{Limit: 2})
	require.NoError(t, err)
	require.Len(t, result1.Items, 2)
	require.True(t, result1.HasMore)
	require.NotEmpty(t, result1.NextCursor)

	result2, err := svc.Search(t.Context(), tenantID, rapi.SearchQuery{Limit: 2, Cursor: result1.NextCursor})
	require.NoError(t, err)
	require.Len(t, result2.Items, 1)
	require.False(t, result2.HasMore)
	require.Empty(t, result2.NextCursor)

	// No overlap: result2's single item must not appear in result1.
	require.NotEqual(t, result1.Items[0].RecordingID, result2.Items[0].RecordingID)
	require.NotEqual(t, result1.Items[1].RecordingID, result2.Items[0].RecordingID)
}

// TestService_Search_BadCursorReturnsInvalidInput verifies that a malformed
// cursor string folds into ErrInvalidInput (HTTP 400).
func TestService_Search_BadCursorReturnsInvalidInput(t *testing.T) {
	t.Parallel()
	// No DB needed — cursor validation fires before store is called.
	pool := startPGContainer(t)
	svc := buildService(t, pool)

	_, err := svc.Search(t.Context(), uuid.Must(uuid.NewV7()), rapi.SearchQuery{Cursor: "not-base64-!@#"})
	require.ErrorIs(t, err, rapi.ErrInvalidInput)
}

// TestService_Search_BadStatusReturnsInvalidInput verifies that an unknown
// status string folds into ErrInvalidInput before any DB round-trip.
func TestService_Search_BadStatusReturnsInvalidInput(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	svc := buildService(t, pool)

	_, err := svc.Search(t.Context(), uuid.Must(uuid.NewV7()), rapi.SearchQuery{
		Status: []string{"bogus"},
		Limit:  10,
	})
	require.ErrorIs(t, err, rapi.ErrInvalidInput)
}

// TestService_Search_ProjectIDFilter seeds 2 projects within one tenant,
// commits recordings under each, and asserts that filtering by project_A
// returns only its rows.
func TestService_Search_ProjectIDFilter(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	svc := buildService(t, pool)

	// Seed project A: 2 calls + recordings.
	projectA := uuid.Must(uuid.NewV7())
	callA1 := seedCallInTenantWithProject(t, pool, tenantID, projectA)
	callA2 := seedCallInTenantWithProject(t, pool, tenantID, projectA)
	// Seed project B: 1 call + recording (should be excluded).
	projectB := uuid.Must(uuid.NewV7())
	callB1 := seedCallInTenantWithProject(t, pool, tenantID, projectB)

	for _, callID := range []uuid.UUID{callA1, callA2, callB1} {
		_, err := svc.Commit(t.Context(), newCommitInput(t, tenantID, callID))
		require.NoError(t, err)
	}

	result, err := svc.Search(t.Context(), tenantID, rapi.SearchQuery{
		ProjectID: &projectA,
		Limit:     10,
	})
	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	for _, item := range result.Items {
		require.NotEqual(t, callB1, item.CallID, "result must not include projectB's call")
	}
}

// TestService_Search_LimitClampedTo200 verifies that Limit=999 is silently
// clamped to 200 and does not return an error.
func TestService_Search_LimitClampedTo200(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	svc := buildService(t, pool)

	// Seed 5 recordings — well under 200 — to confirm clamping doesn't error
	// and returns however many rows exist.
	for range 5 {
		callID := seedCallInTenant(t, pool, tenantID)
		_, err := svc.Commit(t.Context(), newCommitInput(t, tenantID, callID))
		require.NoError(t, err)
	}

	result, err := svc.Search(t.Context(), tenantID, rapi.SearchQuery{Limit: 999})
	require.NoError(t, err)
	require.LessOrEqual(t, len(result.Items), 200)
	// All 5 seeded rows are returned (999 clamped to 200, which is > 5).
	require.Len(t, result.Items, 5)
}

// TestService_Search_LimitDefault50 verifies that Limit=0 defaults to 50.
// Seeds 3 recordings: expects all 3 back (not 0), confirming the default
// is not zero.
func TestService_Search_LimitDefault50(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	svc := buildService(t, pool)

	for range 3 {
		callID := seedCallInTenant(t, pool, tenantID)
		_, err := svc.Commit(t.Context(), newCommitInput(t, tenantID, callID))
		require.NoError(t, err)
	}

	result, err := svc.Search(t.Context(), tenantID, rapi.SearchQuery{Limit: 0})
	require.NoError(t, err)
	// Default=50 > 3 seeded rows — all must be returned.
	require.Len(t, result.Items, 3)
	require.False(t, result.HasMore)
}

// TestService_Search_TenantIsolation seeds two tenants with different recording
// counts and asserts that searching tenant A never leaks tenant B's rows.
func TestService_Search_TenantIsolation(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantA := seedTenant(t, pool)
	// Use a distinct seedTenantFull helper to avoid same-millisecond org_code collision.
	tenantB := seedTenantFull(t, pool)
	svcA := buildService(t, pool)

	// Seed 2 recordings under tenant A.
	for range 2 {
		callID := seedCallInTenant(t, pool, tenantA)
		_, err := svcA.Commit(t.Context(), newCommitInput(t, tenantA, callID))
		require.NoError(t, err)
	}
	// Seed 1 recording under tenant B.
	callB := seedCallInTenant(t, pool, tenantB)
	_, err := svcA.Commit(t.Context(), newCommitInput(t, tenantB, callB))
	require.NoError(t, err)

	result, err := svcA.Search(t.Context(), tenantA, rapi.SearchQuery{Limit: 20})
	require.NoError(t, err)
	require.Len(t, result.Items, 2, "tenant A must see exactly its 2 recordings")
	for _, item := range result.Items {
		require.Equal(t, tenantA, item.TenantID, "result must not contain tenant B's recording")
	}
}

// ────────── local seed helpers ──────────

// seedCallInTenantWithProject inserts a project (if not already present)
// and a call under that project inside an existing tenant. Returns the
// new call_id. The project INSERT uses ON CONFLICT DO NOTHING so the
// caller can reuse the same projectID across multiple calls.
func seedCallInTenantWithProject(t *testing.T, pool *postgres.Pool, tenantID, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	callID := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status)
			 VALUES ($1, $2, $3, 'Test Project', 'active')
			 ON CONFLICT (id) DO NOTHING`,
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

// seedTenantFull is defined in service_test.go as part of the existing
// test helpers — import it via the package-level test binary by declaring
// the function here only if it doesn't already exist.
// NOTE: seedTenantFull lives in service_test.go from Plan 12.3 store tests
// but is NOT present in service/service_test.go. Define it here.
func seedTenantFull(t *testing.T, pool *postgres.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			 VALUES ($1, $2, $3, 'active', 'kms-test', '\x00')`,
			id,
			"org-"+id.String(),
			"tenant-"+id.String(),
		)
		return err
	}))
	return id
}

//go:build integration

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TestPostgresStore_LookupTenant_Found verifies a BypassRLS SELECT
// returns the tenant for a call_id whose recording row exists.
func TestPostgresStore_LookupTenant_Found(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantID := seedTenantFull(t, pool)
	callID := seedCallInTenant(t, pool, tenantID)

	// Insert recording row inside WithTenant Tx.
	row := newRow(t, tenantID, callID)
	row.Status = "stored"
	row.ColdAt = time.Now().UTC().Add(time.Hour)
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	got, err := st.LookupTenant(t.Context(), callID)
	require.NoError(t, err)
	assert.Equal(t, tenantID, got)
}

// TestPostgresStore_LookupTenant_NotFound verifies the wrapped
// store.ErrCallNotFound for a call_id with no matching row.
func TestPostgresStore_LookupTenant_NotFound(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	_, err := st.LookupTenant(t.Context(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrCallNotFound),
		"missing call_id must wrap ErrCallNotFound")
}

// TestPostgresStore_LookupTenant_BypassRLS_CrossTenant verifies that
// the lookup works regardless of any caller-set tenant context — this
// is the property the realtime CallResolver needs (the WS subscriber
// claims tenant B, the call belongs to tenant A; the lookup must
// still resolve so TopicRBAC can detect the mismatch).
func TestPostgresStore_LookupTenant_BypassRLS_CrossTenant(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)

	tenantA := seedTenantFull(t, pool)
	tenantB := seedTenantFull(t, pool)
	callA := seedCallInTenant(t, pool, tenantA)

	rowA := newRow(t, tenantA, callA)
	rowA.Status = "stored"
	rowA.ColdAt = time.Now().UTC().Add(time.Hour)
	require.NoError(t, pool.WithTenant(t.Context(), tenantA, func(tx postgres.Tx) error {
		_, _, err := st.InsertRecordingIdempotent(t.Context(), tx, rowA)
		return err
	}))

	// Caller's ambient WithTenant context is tenantB (would normally
	// hide tenantA's row under RLS); LookupTenant uses BypassRLS so
	// it must still resolve.
	require.NoError(t, pool.WithTenant(t.Context(), tenantB, func(_ postgres.Tx) error {
		got, err := st.LookupTenant(t.Context(), callA)
		require.NoError(t, err)
		assert.Equal(t, tenantA, got)
		return nil
	}))
}

// Compile-time interface check kept here in the test file: store ↔ api
// import direction is store → api, so the assertion lives in the test
// where both packages are imported anyway.
var _ rapi.CallTenantLookup = (*store.PostgresStore)(nil)

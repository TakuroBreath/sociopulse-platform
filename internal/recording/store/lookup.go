// lookup.go provides the call_id → tenant_id BypassRLS read for the
// realtime CallResolver (Plan 11.4 Task 7). Lives in a separate file
// from postgres.go (per-tenant CRUD) and lifecycle.go (Plan 12.4
// retention sweeps) because the use case is distinct: a single
// cross-tenant pinpoint read keyed only by call_id.
//
// Plan 11.4 Task 4.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sociopulse/platform/pkg/postgres"
)

// LookupTenant returns the tenant_id for the row with the given
// call_id. Implements internal/recording/api.CallTenantLookup.
//
// Runs inside pool.BypassRLS — the use case is a cross-tenant resolver
// where the caller does not yet know the tenant. tenancy_admin has
// SELECT on call_recordings (migration 000011_admin_grants_call_recordings;
// Plan 12.4 Task 1 added it). This is verified at runtime: the SELECT
// would fail with permission-denied if the grant were missing.
//
// Returns wrapped ErrCallNotFound on no matching row.
func (s *PostgresStore) LookupTenant(ctx context.Context, callID uuid.UUID) (uuid.UUID, error) {
	if callID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("recording.store.LookupTenant: nil callID")
	}

	var tenantID uuid.UUID
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		const q = `SELECT tenant_id FROM call_recordings WHERE call_id = $1 LIMIT 1`
		row := tx.QueryRow(ctx, q, callID)
		switch err := row.Scan(&tenantID); {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrCallNotFound
		case err != nil:
			return fmt.Errorf("recording.store.LookupTenant: %w", err)
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

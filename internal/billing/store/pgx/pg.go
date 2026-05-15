// Package pgx is the pgx-backed billing store. It implements the
// service.SettingsBackend interface so internal/billing/service.TariffStore
// can read and write tenant_settings rows for the keys it owns
// (billing.trunks / billing.wage_per_survey / etc.).
//
// Adapters here are tenant-scoped via *postgres.Pool.WithTenant — every
// query runs inside RLS so a forged tenant_id from upstream gets denied
// by the tenant_settings_iso policy (migration 000001_init). The
// Get/Upsert split mirrors tenant_settings's (tenant_id, key) PRIMARY
// KEY: reads are single-row, writes batch in a single transaction so
// the multi-key tariff update is atomic.
package pgx

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/sociopulse/platform/pkg/postgres"
)

// PG is the billing pgx-backed store. It holds the pool by reference;
// callers manage the pool's lifecycle.
type PG struct {
	pool *postgres.Pool
}

// New returns a PG bound to the pool. The pool's tenant scoping is
// applied at call time via WithTenant — the store does not cache it.
// Panics on a nil pool: every caller is constructed at module-register
// time when the pool is mandatory; a nil here is a wiring bug we want
// to fail loudly rather than degrade silently.
func New(p *postgres.Pool) *PG {
	if p == nil {
		panic("billing/store/pgx.New: pool must be non-nil")
	}
	return &PG{pool: p}
}

// GetSetting reads a single tenant_settings row by (tenant_id, key).
// Returns pgx.ErrNoRows when the row is absent so the service layer can
// errors.Is-discriminate "missing" from "broken".
//
// The value is selected via ::text cast so the returned []byte is the
// raw JSON document (parsed by the caller). RLS is enforced by the
// WithTenant transaction; a leaked cross-tenant call will return
// ErrNoRows rather than another tenant's row.
func (s *PG) GetSetting(ctx context.Context, tenantID uuid.UUID, key string) ([]byte, error) {
	var raw []byte
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx,
			`select value::text from tenant_settings where tenant_id = $1 and key = $2`,
			tenantID, key,
		).Scan(&raw)
	})
	if err != nil {
		if errors.Is(err, pgxv5.ErrNoRows) {
			// Re-return the sentinel directly (unwrapped) so the caller's
			// errors.Is check works regardless of which layer wraps it.
			return nil, pgxv5.ErrNoRows
		}
		return nil, fmt.Errorf("billing/store: get setting %s: %w", key, err)
	}
	return raw, nil
}

// UpsertSettings writes multiple keys for a single tenant in one
// transaction. Partial-write semantics on error: if the Tx aborts, no
// keys are persisted. The map order is undefined; callers must not rely
// on per-key write ordering.
//
// Each value is JSON written via $3::jsonb so Postgres validates the
// document at INSERT time — a malformed payload from the service layer
// surfaces here as a SQL error rather than being silently stored.
func (s *PG) UpsertSettings(ctx context.Context, tenantID uuid.UUID, kv map[string][]byte) error {
	if len(kv) == 0 {
		return nil
	}
	return s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		const q = `
insert into tenant_settings (tenant_id, key, value, updated_at)
values ($1, $2, $3::jsonb, now())
on conflict (tenant_id, key) do update
   set value = excluded.value, updated_at = now()`
		for k, v := range kv {
			if _, err := tx.Exec(ctx, q, tenantID, k, string(v)); err != nil {
				return fmt.Errorf("billing/store: upsert %s: %w", k, err)
			}
		}
		return nil
	})
}

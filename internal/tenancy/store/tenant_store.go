package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// pgUniqueViolation is the SQLSTATE code Postgres returns for a unique-
// constraint violation. We translate it to api.ErrAlreadyExists so callers
// can errors.Is without depending on pgx error codes.
const pgUniqueViolation = "23505"

// PostgresStore is the Postgres-backed implementation of api.Store.
//
// Mutating methods accept a postgres.Tx so the service layer can co-locate
// the row write with a transactional outbox Append (and an eventual audit
// row) in the same transaction. Read methods open a short-lived BypassRLS
// transaction internally so callers can use them without owning a tx.
//
// Application code outside this package must NOT import this struct
// directly — depguard enforces that other modules go through
// internal/tenancy/api.Tenancy.
type PostgresStore struct {
	pool *postgres.Pool
}

// Compile-time assertion that PostgresStore satisfies api.Store.
var _ api.Store = (*PostgresStore)(nil)

// NewPostgresStore constructs a PostgresStore. The pool MUST be the
// project-wide *postgres.Pool — read methods call pool.BypassRLS, which
// sets the tenancy_admin role inside each transaction.
func NewPostgresStore(pool *postgres.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// Insert implements api.Store.Insert inside the caller's tx.
func (s *PostgresStore) Insert(ctx context.Context, tx postgres.Tx, t api.Tenant) (api.Tenant, error) {
	const q = `
		INSERT INTO tenants (org_code, name, status, kms_kek_id, phone_hash_pepper)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`

	var (
		id        uuid.UUID
		createdAt = t.CreatedAt
	)
	err := tx.QueryRow(ctx, q,
		t.OrgCode, t.Name, string(t.Status), t.KMSKEKID, t.PhoneHashPepper,
	).Scan(&id, &createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return api.Tenant{}, fmt.Errorf("%w: %s", api.ErrAlreadyExists, pgErr.ConstraintName)
		}
		return api.Tenant{}, fmt.Errorf("tenancy/store: insert tenant: %w", err)
	}

	t.ID = id
	t.CreatedAt = createdAt
	return t, nil
}

// UpdateStatus implements api.Store.UpdateStatus inside the caller's tx.
func (s *PostgresStore) UpdateStatus(ctx context.Context, tx postgres.Tx, id uuid.UUID, status api.TenantStatus) error {
	const q = `UPDATE tenants SET status = $1 WHERE id = $2`

	tag, err := tx.Exec(ctx, q, string(status), id)
	if err != nil {
		return fmt.Errorf("tenancy/store: update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrNotFound
	}
	return nil
}

// UpsertSetting implements api.Store.UpsertSetting inside the caller's tx.
func (s *PostgresStore) UpsertSetting(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string, value api.SettingValue) error {
	const q = `
		INSERT INTO tenant_settings (tenant_id, key, value, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (tenant_id, key) DO UPDATE
		  SET value = EXCLUDED.value, updated_at = now()`

	if _, err := tx.Exec(ctx, q, tenantID, key, []byte(value.Raw())); err != nil {
		return fmt.Errorf("tenancy/store: upsert setting: %w", err)
	}
	return nil
}

// DeleteSetting implements api.Store.DeleteSetting inside the caller's tx.
func (s *PostgresStore) DeleteSetting(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string) error {
	const q = `DELETE FROM tenant_settings WHERE tenant_id = $1 AND key = $2`

	tag, err := tx.Exec(ctx, q, tenantID, key)
	if err != nil {
		return fmt.Errorf("tenancy/store: delete setting: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrNotFound
	}
	return nil
}

// Get implements api.Store.Get. Read-only; uses an internal BypassRLS tx.
func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (api.Tenant, error) {
	const q = `
		SELECT id, org_code, name, status, kms_kek_id, phone_hash_pepper, created_at
		FROM tenants WHERE id = $1`

	var (
		t      api.Tenant
		status string
	)
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, q, id).Scan(
			&t.ID, &t.OrgCode, &t.Name, &status, &t.KMSKEKID, &t.PhoneHashPepper, &t.CreatedAt,
		)
	})
	if errors.Is(err, postgres.ErrNoRows) {
		return api.Tenant{}, api.ErrNotFound
	}
	if err != nil {
		return api.Tenant{}, fmt.Errorf("tenancy/store: select tenant: %w", err)
	}
	t.Status = api.TenantStatus(status)
	return t, nil
}

// GetByOrgCode implements api.Store.GetByOrgCode. Read-only; uses an
// internal BypassRLS tx.
func (s *PostgresStore) GetByOrgCode(ctx context.Context, orgCode string) (api.Tenant, error) {
	const q = `
		SELECT id, org_code, name, status, kms_kek_id, phone_hash_pepper, created_at
		FROM tenants WHERE org_code = $1`

	var (
		t      api.Tenant
		status string
	)
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, q, orgCode).Scan(
			&t.ID, &t.OrgCode, &t.Name, &status, &t.KMSKEKID, &t.PhoneHashPepper, &t.CreatedAt,
		)
	})
	if errors.Is(err, postgres.ErrNoRows) {
		return api.Tenant{}, api.ErrNotFound
	}
	if err != nil {
		return api.Tenant{}, fmt.Errorf("tenancy/store: select tenant by org_code: %w", err)
	}
	t.Status = api.TenantStatus(status)
	return t, nil
}

// List implements api.Store.List. Read-only; uses an internal BypassRLS tx.
func (s *PostgresStore) List(ctx context.Context, f api.ListTenantsFilter) ([]api.Tenant, error) {
	const q = `
		SELECT id, org_code, name, status, kms_kek_id, phone_hash_pepper, created_at
		FROM tenants
		WHERE ($1::text IS NULL OR status = $1)
		  AND ($2::text = '' OR org_code = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3 OFFSET $4`

	var statusArg any
	if f.Status != nil {
		statusArg = string(*f.Status)
	}

	var out []api.Tenant
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, q, statusArg, f.OrgCode, f.Limit, f.Offset)
		if err != nil {
			return fmt.Errorf("query: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				t      api.Tenant
				status string
			)
			if err := rows.Scan(
				&t.ID, &t.OrgCode, &t.Name, &status,
				&t.KMSKEKID, &t.PhoneHashPepper, &t.CreatedAt,
			); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			t.Status = api.TenantStatus(status)
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tenancy/store: list tenants: %w", err)
	}
	return out, nil
}

// GetPhoneHashPepper implements api.Store.GetPhoneHashPepper.
func (s *PostgresStore) GetPhoneHashPepper(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	const q = `SELECT phone_hash_pepper FROM tenants WHERE id = $1`

	var pepper []byte
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, q, tenantID).Scan(&pepper)
	})
	if errors.Is(err, postgres.ErrNoRows) {
		return nil, api.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenancy/store: select pepper: %w", err)
	}
	return pepper, nil
}

// GetSetting implements api.Store.GetSetting.
func (s *PostgresStore) GetSetting(ctx context.Context, tenantID uuid.UUID, key string) (api.SettingValue, error) {
	const q = `SELECT value FROM tenant_settings WHERE tenant_id = $1 AND key = $2`

	var raw json.RawMessage
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, q, tenantID, key).Scan(&raw)
	})
	if errors.Is(err, postgres.ErrNoRows) {
		return api.SettingValue{}, api.ErrNotFound
	}
	if err != nil {
		return api.SettingValue{}, fmt.Errorf("tenancy/store: select setting: %w", err)
	}
	return api.SettingValueFromRaw(raw), nil
}

// GetAllSettings implements api.Store.GetAllSettings.
func (s *PostgresStore) GetAllSettings(ctx context.Context, tenantID uuid.UUID) (map[string]api.SettingValue, error) {
	const q = `SELECT key, value FROM tenant_settings WHERE tenant_id = $1`

	out := make(map[string]api.SettingValue)
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return fmt.Errorf("query: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				key string
				raw json.RawMessage
			)
			if err := rows.Scan(&key, &raw); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			out[key] = api.SettingValueFromRaw(raw)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tenancy/store: select all settings: %w", err)
	}
	return out, nil
}

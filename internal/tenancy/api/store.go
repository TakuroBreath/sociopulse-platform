package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// Store is the persistence interface used by TenantService and SettingsCache.
// The concrete implementation (internal/tenancy/store.PostgresStore) connects
// through the `tenancy_admin` BYPASSRLS Postgres role and is therefore the
// only path in the codebase that may safely read or write across tenants.
//
// Mutating methods accept a postgres.Tx so the service layer can co-locate
// the row write with a transactional-outbox Append (and a future audit log
// row) in the same transaction. Read methods are tx-less for ergonomics —
// the store opens a short-lived BypassRLS transaction internally.
//
// Declared here in api/ so test doubles in module-internal tests can
// satisfy it without importing the internal store package.
type Store interface {
	// Insert persists t inside tx and returns it with ID and CreatedAt
	// populated. Returns ErrAlreadyExists if OrgCode collides with an
	// existing row.
	Insert(ctx context.Context, tx postgres.Tx, t Tenant) (Tenant, error)

	// UpdateStatus transitions a tenant to a new status inside tx. Returns
	// ErrNotFound if the row does not exist.
	UpdateStatus(ctx context.Context, tx postgres.Tx, id uuid.UUID, status TenantStatus) error

	// UpsertSetting inserts or updates the (tenantID, key) row inside tx.
	UpsertSetting(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string, value SettingValue) error

	// DeleteSetting removes the (tenantID, key) row inside tx, or
	// ErrNotFound when missing.
	DeleteSetting(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string) error

	// Get returns the tenant with the given ID, or ErrNotFound. Read-only;
	// the store opens a short-lived BypassRLS transaction internally.
	Get(ctx context.Context, id uuid.UUID) (Tenant, error)

	// GetByOrgCode resolves a tenant by its public OrgCode, or ErrNotFound.
	GetByOrgCode(ctx context.Context, orgCode string) (Tenant, error)

	// List returns tenants matching filter, ordered by created_at DESC.
	// The caller (TenantService) clamps Limit/Offset before invocation.
	List(ctx context.Context, filter ListTenantsFilter) ([]Tenant, error)

	// GetPhoneHashPepper returns the per-tenant pepper used by PhoneHasher.
	// Returns ErrNotFound if the tenant does not exist.
	GetPhoneHashPepper(ctx context.Context, tenantID uuid.UUID) ([]byte, error)

	// GetSetting returns the value for (tenantID, key), or ErrNotFound.
	GetSetting(ctx context.Context, tenantID uuid.UUID, key string) (SettingValue, error)

	// GetAllSettings returns every setting for the tenant as a snapshot.
	GetAllSettings(ctx context.Context, tenantID uuid.UUID) (map[string]SettingValue, error)
}

// SettingsPublisher abstracts the message-bus emission of lifecycle and
// cache-invalidation events so the service layer is testable without NATS.
//
// In Plan 04 Task 2 the canonical write path uses the transactional outbox
// (pkg/outbox) for durability; SettingsPublisher remains for non-Postgres-
// coupled paths and for the SettingsCache invalidation surface used by
// later tasks.
type SettingsPublisher interface {
	// PublishCreated emits tenant.<id>.created.
	PublishCreated(ctx context.Context, t Tenant) error
	// PublishSuspended emits tenant.<id>.suspended.
	PublishSuspended(ctx context.Context, tenantID uuid.UUID) error
	// PublishArchived emits tenant.<id>.archived.
	PublishArchived(ctx context.Context, tenantID uuid.UUID) error
	// PublishSettingUpdated emits tenant.<id>.settings.updated for an upsert.
	PublishSettingUpdated(ctx context.Context, tenantID uuid.UUID, key string) error
	// PublishSettingDeleted emits tenant.<id>.settings.updated for a delete.
	PublishSettingDeleted(ctx context.Context, tenantID uuid.UUID, key string) error
}

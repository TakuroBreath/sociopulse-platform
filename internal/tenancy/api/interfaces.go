package api

import (
	"context"

	"github.com/google/uuid"
)

// TenantService is the public CRUD surface for tenants. Service-Owner endpoints
// in cmd/api delegate to this interface.
type TenantService interface {
	// Create allocates a new tenant row and provisions its KMS KEK + S3 bucket.
	Create(ctx context.Context, req CreateTenantRequest) (Tenant, error)
	// Get returns the tenant with the given ID, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Tenant, error)
	// GetByOrgCode resolves a tenant by its public OrgCode (used at login).
	GetByOrgCode(ctx context.Context, orgCode string) (Tenant, error)
	// List returns tenants matching filter; the implementation enforces RBAC.
	List(ctx context.Context, filter ListTenantsFilter) ([]Tenant, error)
	// Suspend transitions a tenant to TenantStatusSuspended with a reason.
	Suspend(ctx context.Context, id uuid.UUID, reason string) error
	// Resume transitions a tenant from TenantStatusSuspended back to active.
	Resume(ctx context.Context, id uuid.UUID) error
	// Archive transitions a tenant to TenantStatusArchived (terminal).
	Archive(ctx context.Context, id uuid.UUID) error
}

// KMSResolver is the envelope-encryption surface used by every module that
// stores tenant-private payloads (recordings, surveys, audit) in S3 / Postgres.
type KMSResolver interface {
	// EnsureKEK lazily provisions a Yandex KMS symmetric key for the tenant
	// and returns its ID. Subsequent calls are cheap (cached).
	EnsureKEK(ctx context.Context, tenantID uuid.UUID) (kekID string, err error)
	// GenerateDataKey returns a new DEK wrapped under the tenant's KEK.
	GenerateDataKey(ctx context.Context, tenantID uuid.UUID) (DataKey, error)
	// Encrypt wraps plaintext under the tenant's KEK directly. Use only for
	// small payloads (< 4 KiB); larger payloads must go through GenerateDataKey + AES-GCM.
	Encrypt(ctx context.Context, tenantID uuid.UUID, plaintext []byte) ([]byte, error)
	// Decrypt unwraps ciphertext previously produced by Encrypt.
	Decrypt(ctx context.Context, tenantID uuid.UUID, ciphertext []byte) ([]byte, error)
	// InvalidateCache forgets cached KEKs for the tenant. Called on KEK rotation.
	InvalidateCache(tenantID uuid.UUID)
}

// PhoneHasher computes deterministic, tenant-scoped HMAC-SHA256 hashes for
// respondent phone numbers. The pepper is per-tenant so a global rainbow
// table is useless against any single tenant.
type PhoneHasher interface {
	// Hash returns the 32-byte HMAC-SHA256 of normalised phone with the
	// tenant pepper as key. Returns ErrInvalidArgument for bad input.
	Hash(ctx context.Context, tenantID uuid.UUID, phone string) ([]byte, error)
	// Normalise canonicalises a phone string to E.164 (Russia in v1).
	Normalise(phone string) (string, error)
}

// SettingsCache is the read/write surface for tenant_settings rows.
// Implementations cache locally and invalidate on tenant.<t>.settings.updated.
type SettingsCache interface {
	// Get returns the value for key, or ErrNotFound.
	Get(ctx context.Context, tenantID uuid.UUID, key string) (SettingValue, error)
	// GetWithDefault returns the value for key, falling back to def if missing.
	GetWithDefault(ctx context.Context, tenantID uuid.UUID, key string, def SettingValue) (SettingValue, error)
	// GetAll returns every setting for the tenant (snapshot).
	GetAll(ctx context.Context, tenantID uuid.UUID) (map[string]SettingValue, error)
	// Set upserts a setting and publishes settings.updated for peer invalidation.
	Set(ctx context.Context, tenantID uuid.UUID, key string, value SettingValue) error
	// Delete removes a setting and publishes settings.updated.
	Delete(ctx context.Context, tenantID uuid.UUID, key string) error
	// InvalidateLocal evicts a single key from the local cache without DB I/O.
	// Called by the NATS subscriber on a peer's update notification.
	InvalidateLocal(tenantID uuid.UUID, key string)
	// InvalidateAllLocal evicts every key for a tenant from the local cache.
	InvalidateAllLocal(tenantID uuid.UUID)
}

// BucketProvisioner ensures the per-tenant S3 bucket exists with the
// expected lifecycle policy and KMS encryption settings.
type BucketProvisioner interface {
	// EnsureBucket creates the bucket if it does not exist and returns its name.
	// Idempotent — safe to call on every API request.
	EnsureBucket(ctx context.Context, tenantID uuid.UUID) (bucket string, err error)
}

// Tenancy is the aggregate exposed to other modules so they can take a single
// dependency rather than four. The four sub-interfaces both define a method
// named "Get" with different signatures, so embedding them directly would
// produce a Go ambiguous-method-set error. Instead, Tenancy exposes the four
// sub-surfaces via getters; concrete adapters return themselves cast to each
// sub-interface (since one struct typically implements all four).
type Tenancy interface {
	// Tenants returns the TenantService surface for tenant CRUD.
	Tenants() TenantService
	// Settings returns the SettingsCache surface for tenant_settings.
	Settings() SettingsCache
	// KMS returns the KMSResolver surface for envelope encryption.
	KMS() KMSResolver
	// Phones returns the PhoneHasher surface for HMAC phone hashing.
	Phones() PhoneHasher
}

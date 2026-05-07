package api

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// TenantStatus is the lifecycle state of a tenant.
type TenantStatus string

const (
	// TenantStatusActive is the only state in which data-plane ops succeed.
	TenantStatusActive TenantStatus = "active"
	// TenantStatusSuspended blocks data-plane ops but keeps the tenant intact.
	TenantStatusSuspended TenantStatus = "suspended"
	// TenantStatusArchived is the terminal state (read-only graveyard).
	TenantStatusArchived TenantStatus = "archived"
)

// Valid reports whether s is a known status.
func (s TenantStatus) Valid() bool {
	switch s {
	case TenantStatusActive, TenantStatusSuspended, TenantStatusArchived:
		return true
	}
	return false
}

// Tenant is the public DTO for a tenant row.
//
// PhoneHashPepper is *not* exposed in any external response. The JSON tag is
// "-" so it never leaks into HTTP/JSON output; the field is populated only
// inside the tenancy module (PhoneHasher / store-layer queries). Other
// modules read peppers through PhoneHasher, never through this struct.
type Tenant struct {
	ID              uuid.UUID    `json:"id"`
	OrgCode         string       `json:"org_code"` // e.g. "CC-MOSKVA-01"
	Name            string       `json:"name"`     // e.g. "ВЦИОМ-Москва"
	Status          TenantStatus `json:"status"`
	KMSKEKID        string       `json:"kms_kek_id"` // Yandex KMS symmetric key ID
	PhoneHashPepper []byte       `json:"-"`          // 32 random bytes; never serialised
	// RecordingBucket is the per-tenant Object Storage bucket used for call
	// recordings. Populated after BucketProvisioner.Provision succeeds; empty
	// while ErrBucketProvisionPending is in effect.
	RecordingBucket string    `json:"recording_bucket,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// Validate enforces the invariants that aren't already enforced by the DB
// constraint set in Plan 03 — used in service-layer guards before INSERT/UPDATE.
func (t Tenant) Validate() error {
	if t.OrgCode == "" {
		return fmt.Errorf("%w: org_code must be non-empty", ErrInvalidArgument)
	}
	if len(t.OrgCode) > 64 {
		return fmt.Errorf("%w: org_code must be <= 64 chars", ErrInvalidArgument)
	}
	if t.Name == "" {
		return fmt.Errorf("%w: name must be non-empty", ErrInvalidArgument)
	}
	if !t.Status.Valid() {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidArgument, t.Status)
	}
	return nil
}

// CreateTenantRequest is what a Service-Owner POSTs to /admin/tenants.
type CreateTenantRequest struct {
	OrgCode string `json:"org_code"`
	Name    string `json:"name"`
}

// ListTenantsFilter narrows TenantService.List output. All fields optional.
type ListTenantsFilter struct {
	Status  *TenantStatus
	OrgCode string // exact match if non-empty
	Limit   int    // default 50, max 500
	Offset  int
}

// TenantService is the cross-tenant CRUD surface. The implementation talks
// to Postgres via the `tenancy_admin` BYPASSRLS role. ALL data-plane modules
// must NOT use this interface directly — they look up tenants via the per-
// request middleware (auth module) which caches a Tenant for the request.
//
// Mutating methods publish a NATS event:
//   - Create   → tenant.<id>.created
//   - Suspend  → tenant.<id>.suspended
//   - Resume   → tenant.<id>.resumed
//   - Archive  → tenant.<id>.archived
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

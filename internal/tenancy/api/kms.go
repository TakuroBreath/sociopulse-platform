package api

import (
	"context"

	"github.com/google/uuid"
)

// DataKey is the result of GenerateDataKey: plaintext for immediate use,
// ciphertext for storage alongside the encrypted payload.
//
// CRITICAL: the caller must zeroise Plaintext after use:
//
//	defer func() {
//	    for i := range dk.Plaintext {
//	        dk.Plaintext[i] = 0
//	    }
//	}()
type DataKey struct {
	Plaintext  []byte // 32 bytes for AES-256
	Ciphertext []byte // KMS-encrypted blob, store with the payload
	KeyVersion string // KEK version that wrapped this DEK (for rotation tracking)
}

// KMSResolver wraps Yandex KMS for the tenancy module. Other modules (recording,
// auth, crm) consume this through the api.Tenancy aggregate.
//
// All methods are idempotent on the KMS side: retries are safe.
//
// Plan 13.2.5 Task 6: Encrypt/Decrypt take `scope` and `rowID` arguments
// that are bound into the AEAD authentication tag via
// pkg/encryption.BuildAAD. An attacker who tries to splice a ciphertext
// from one (scope, row) to another fails AES-GCM Open. Backward
// compatibility with existing production ciphertexts (no AAD bind) is
// preserved via a version byte on the wrapped-DEK envelope — see
// internal/tenancy/service/kms_resolver.go for the layout.
type KMSResolver interface {
	// EnsureKEK creates a per-tenant KEK in Yandex KMS if absent and returns its ID.
	// Idempotent: safe to call repeatedly during onboarding.
	EnsureKEK(ctx context.Context, tenantID uuid.UUID) (kekID string, err error)

	// GenerateDataKey produces a fresh DEK wrapped by the tenant's KEK.
	// Use the plaintext to encrypt a single payload, store the ciphertext alongside.
	GenerateDataKey(ctx context.Context, tenantID uuid.UUID) (DataKey, error)

	// Encrypt performs in-app AES-256-GCM with a cached DEK (for short PII like phones).
	// scope and rowID identify the column / row owning the ciphertext; both
	// are bound into the AEAD AAD via pkg/encryption.BuildAAD so a row /
	// column swap fails authentication. See package doc on BuildAAD for the
	// canonical scope strings (e.g. "auth.user.phone", "crm.respondent.phone").
	// Returns ciphertext that includes a version byte, the wrapped-DEK
	// header, and the AES-GCM blob.
	Encrypt(ctx context.Context, tenantID uuid.UUID, scope, rowID string, plaintext []byte) ([]byte, error)

	// Decrypt reverses Encrypt. Resolves the DEK via the cache, transparently
	// invokes KMS.Decrypt on cache miss. scope and rowID MUST match the
	// values supplied to Encrypt; otherwise AEAD authentication fails and
	// the resolver surfaces ErrInvalidArgument.
	//
	// Legacy (pre-Plan-13.2.5) ciphertexts decrypt unchanged — the resolver
	// detects them by inspecting the leading version byte and skips the
	// AAD bind. The scope/rowID arguments are ignored on the legacy path.
	Decrypt(ctx context.Context, tenantID uuid.UUID, scope, rowID string, ciphertext []byte) ([]byte, error)

	// InvalidateCache drops the in-memory DEK cache entry for the tenant.
	// Called after KEK rotation or tenant suspension.
	InvalidateCache(tenantID uuid.UUID)
}

// BucketProvisioner manages per-tenant Object Storage buckets used for
// recordings. The implementation MUST be idempotent: re-calling Provision
// for an existing tenant returns success without recreating the bucket.
//
// Bucket naming: implementations use a deterministic prefix so an operator
// can locate a tenant's bucket without consulting the database. The exact
// prefix is implementation-defined; today it is "sociopulse-recordings-<id>".
//
// Security invariants enforced by the implementation:
//   - Default SSE is enabled (AES-256 or SSE-KMS keyed on the tenant KEK).
//   - Public access is denied (bucket policy + ACL).
//   - The bucket lifecycle is configured per spec §9.4 (hot then cold tier).
type BucketProvisioner interface {
	// Provision creates (if absent) the recordings bucket for tenant `id`,
	// configures SSE with the tenant's KEK, applies a per-tenant IAM
	// policy that limits s3:GetObject/s3:PutObject to the tenant's
	// service account, and returns the bucket name. Idempotent — safe to
	// call repeatedly during onboarding or via the /admin/repair surface.
	Provision(ctx context.Context, tenantID uuid.UUID, kmsKeyID string) (bucketName string, err error)

	// Decommission marks the bucket for deletion. Real DELETE happens via
	// a separate worker after the grace period — synchronous deletion of
	// recordings is never performed from a TenantService call.
	Decommission(ctx context.Context, tenantID uuid.UUID) error
}

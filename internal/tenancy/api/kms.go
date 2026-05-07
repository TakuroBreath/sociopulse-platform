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
type KMSResolver interface {
	// EnsureKEK creates a per-tenant KEK in Yandex KMS if absent and returns its ID.
	// Idempotent: safe to call repeatedly during onboarding.
	EnsureKEK(ctx context.Context, tenantID uuid.UUID) (kekID string, err error)

	// GenerateDataKey produces a fresh DEK wrapped by the tenant's KEK.
	// Use the plaintext to encrypt a single payload, store the ciphertext alongside.
	GenerateDataKey(ctx context.Context, tenantID uuid.UUID) (DataKey, error)

	// Encrypt performs in-app AES-256-GCM with a cached DEK (for short PII like phones).
	// Returns ciphertext that includes the nonce and a header identifying the wrapped DEK.
	Encrypt(ctx context.Context, tenantID uuid.UUID, plaintext []byte) ([]byte, error)

	// Decrypt reverses Encrypt. Resolves the DEK via the cache, transparently
	// invokes KMS.Decrypt on cache miss.
	Decrypt(ctx context.Context, tenantID uuid.UUID, ciphertext []byte) ([]byte, error)

	// InvalidateCache drops the in-memory DEK cache entry for the tenant.
	// Called after KEK rotation or tenant suspension.
	InvalidateCache(tenantID uuid.UUID)
}

// BucketProvisioner ensures the per-tenant S3 bucket exists with the
// expected lifecycle policy and KMS encryption settings.
type BucketProvisioner interface {
	// EnsureBucket creates the bucket if it does not exist and returns its name.
	// Idempotent — safe to call on every API request.
	EnsureBucket(ctx context.Context, tenantID uuid.UUID) (bucket string, err error)
}

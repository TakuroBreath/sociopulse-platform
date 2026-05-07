package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

// LocalBucketProvisioner is the in-process api.BucketProvisioner adapter
// used by `make dev-up`, unit tests, and any composition that opts out of a
// real Object Storage call. It records bucket state in memory so other
// modules can assert "a bucket exists for this tenant" without standing up
// a MinIO container in fast unit tests.
//
// Security invariants enforced by Provision:
//   - PublicAccess defaults to false (deny public).
//   - SSEEnabled is true and SSEKMSKeyID stores the tenant KEK so the bucket
//     "encrypts at rest with the tenant's key" property is preserved across
//     provider swaps.
//
// Concurrency: the provisioner is safe for concurrent use. Provision is
// idempotent under contention — see TestLocalBucketProvisioner_Concurrent.
type LocalBucketProvisioner struct {
	prefix string

	mu      sync.Mutex
	buckets map[uuid.UUID]LocalBucket
}

// LocalBucket is the in-memory record of a provisioned bucket. The shape
// mirrors the security-relevant fields of an Object Storage bucket so tests
// can assert privacy/encryption invariants without depending on a real
// storage protocol.
type LocalBucket struct {
	// Name is the deterministic bucket name `<prefix><tenant-id>`.
	Name string
	// SSEEnabled is true when default server-side encryption is on.
	SSEEnabled bool
	// SSEKMSKeyID is the KEK ID under which uploads are encrypted (SSE-KMS).
	SSEKMSKeyID string
	// PublicAccess is true ONLY if the bucket policy allows anonymous read.
	// The provisioner always sets this to false.
	PublicAccess bool
	// Decommissioned flips to true after Decommission so the Plan 12
	// retention worker can find tagged-for-deletion buckets.
	Decommissioned bool
	// DecommissionedAt records when Decommission was first invoked.
	// Zero when the bucket is still active.
	DecommissionedAt time.Time
	// CreatedAt records when Provision created the bucket.
	CreatedAt time.Time
}

// NewLocalBucketProvisioner constructs an in-memory provisioner. An empty
// prefix falls back to the canonical "sociopulse-recordings-" so callers
// that forget to set the prefix still produce well-formed bucket names.
func NewLocalBucketProvisioner(prefix string) *LocalBucketProvisioner {
	if prefix == "" {
		prefix = "sociopulse-recordings-"
	}
	return &LocalBucketProvisioner{
		prefix:  prefix,
		buckets: make(map[uuid.UUID]LocalBucket),
	}
}

// Compile-time check: LocalBucketProvisioner must satisfy api.BucketProvisioner.
var _ api.BucketProvisioner = (*LocalBucketProvisioner)(nil)

// Provision creates (if absent) the recordings bucket for `tenantID` and
// returns its deterministic name. Idempotent under concurrent callers.
func (p *LocalBucketProvisioner) Provision(ctx context.Context, tenantID uuid.UUID, kmsKeyID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if tenantID == uuid.Nil {
		return "", fmt.Errorf("%w: tenantID must be a non-zero UUID", api.ErrInvalidArgument)
	}
	if kmsKeyID == "" {
		return "", fmt.Errorf("%w: kmsKeyID must be non-empty (SSE-KMS requires a KEK)", api.ErrInvalidArgument)
	}

	name := p.prefix + tenantID.String()

	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.buckets[tenantID]; ok {
		// Idempotent: re-issue returns the existing bucket. Re-Provision
		// after Decommission is intentionally a no-op too — the operator
		// must clear the decommissioned flag via a separate path.
		return existing.Name, nil
	}

	p.buckets[tenantID] = LocalBucket{
		Name:         name,
		SSEEnabled:   true,
		SSEKMSKeyID:  kmsKeyID,
		PublicAccess: false,
		CreatedAt:    time.Now().UTC(),
	}
	return name, nil
}

// Decommission marks the bucket for deletion. Idempotent: a no-op when the
// bucket does not exist or is already decommissioned. Real DELETE happens
// via the Plan 12 retention worker after the grace period.
func (p *LocalBucketProvisioner) Decommission(ctx context.Context, tenantID uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	bucket, ok := p.buckets[tenantID]
	if !ok {
		return nil
	}
	if !bucket.Decommissioned {
		bucket.Decommissioned = true
		bucket.DecommissionedAt = time.Now().UTC()
		p.buckets[tenantID] = bucket
	}
	return nil
}

// Bucket returns the in-memory bucket record for tenantID. Used by tests
// to assert the security invariants Provision claims to enforce.
//
// Returns the zero-value LocalBucket and false when the tenant has no
// bucket yet.
func (p *LocalBucketProvisioner) Bucket(tenantID uuid.UUID) (LocalBucket, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.buckets[tenantID]
	return b, ok
}

// Count returns the number of buckets the provisioner has created. Used by
// tests to assert idempotency under concurrent callers.
func (p *LocalBucketProvisioner) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buckets)
}

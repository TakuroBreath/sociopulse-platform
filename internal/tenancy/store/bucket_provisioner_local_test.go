package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/store"
)

func TestLocalBucketProvisioner_CreatesIfMissing(t *testing.T) {
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	tenantID := uuid.New()

	name, err := p.Provision(context.Background(), tenantID, "kek-id-1")
	require.NoError(t, err)
	require.Equal(t, "sociopulse-recordings-"+tenantID.String(), name,
		"bucket name must follow the deterministic <prefix><tenant-id> format")
}

func TestLocalBucketProvisioner_NoOpIfExists(t *testing.T) {
	// Idempotency: a second Provision for the same tenant must return the
	// same bucket name without recreating it.
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	tenantID := uuid.New()

	name1, err := p.Provision(context.Background(), tenantID, "kek-id-1")
	require.NoError(t, err)

	name2, err := p.Provision(context.Background(), tenantID, "kek-id-1")
	require.NoError(t, err)
	require.Equal(t, name1, name2, "idempotent Provision must return the same bucket name")
	require.Equal(t, 1, p.Count(),
		"second Provision must NOT create a second bucket entry")
}

func TestLocalBucketProvisioner_BucketIsPrivate(t *testing.T) {
	// The provisioner records "private" ACL by default — public access is
	// always denied. The Local provisioner exposes Bucket() so tests assert
	// the security invariants without coupling to the storage protocol.
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	tenantID := uuid.New()

	_, err := p.Provision(context.Background(), tenantID, "kek-id-1")
	require.NoError(t, err)

	bucket, ok := p.Bucket(tenantID)
	require.True(t, ok, "Bucket lookup must succeed for a freshly-provisioned tenant")
	require.False(t, bucket.PublicAccess, "default bucket policy must deny public access")
}

func TestLocalBucketProvisioner_SSEEnabled(t *testing.T) {
	// SSE invariant: the provisioner records the tenant KEK as the bucket's
	// default encryption key. Recording uploads inherit the SSE setting.
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	tenantID := uuid.New()
	const kekID = "kek-id-42"

	_, err := p.Provision(context.Background(), tenantID, kekID)
	require.NoError(t, err)

	bucket, ok := p.Bucket(tenantID)
	require.True(t, ok)
	require.True(t, bucket.SSEEnabled, "SSE must be enabled by default")
	require.Equal(t, kekID, bucket.SSEKMSKeyID,
		"SSE-KMS must reference the tenant's KEK so recording uploads are encrypted under it")
}

func TestLocalBucketProvisioner_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	_, err := p.Provision(context.Background(), uuid.Nil, "kek-id-1")
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestLocalBucketProvisioner_RejectsEmptyKEKID(t *testing.T) {
	// SSE-KMS requires a KEK ID. Provisioning without one would yield an
	// effectively unencrypted bucket — fail loud so onboarding cannot
	// silently regress.
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	_, err := p.Provision(context.Background(), uuid.New(), "")
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestLocalBucketProvisioner_DefaultPrefix(t *testing.T) {
	// An empty prefix falls back to "sociopulse-recordings-" so callers
	// that forget to set the prefix still get the canonical layout.
	t.Parallel()

	p := store.NewLocalBucketProvisioner("")
	tenantID := uuid.New()

	name, err := p.Provision(context.Background(), tenantID, "kek-id-1")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(name, "sociopulse-recordings-"),
		"empty prefix must default to sociopulse-recordings-, got %q", name)
}

func TestLocalBucketProvisioner_HonoursContextCancellation(t *testing.T) {
	// Context cancellation must surface even from the in-memory dev
	// provisioner so call sites can rely on ctx propagation everywhere.
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Provision(ctx, uuid.New(), "kek-id-1")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled,
		"Provision must return ctx.Err() when the caller cancelled")
}

func TestLocalBucketProvisioner_Decommission_MarksBucket(t *testing.T) {
	// Decommission marks the bucket but never deletes it — Plan 12 retention
	// worker handles the actual DELETE after the grace period. The Local
	// provisioner exposes Bucket(...).Decommissioned for assertion.
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	tenantID := uuid.New()

	_, err := p.Provision(context.Background(), tenantID, "kek-id-1")
	require.NoError(t, err)

	require.NoError(t, p.Decommission(context.Background(), tenantID))

	bucket, ok := p.Bucket(tenantID)
	require.True(t, ok, "Decommission must NOT delete the bucket — only mark it")
	require.True(t, bucket.Decommissioned, "Decommission must flip the marker to true")
}

func TestLocalBucketProvisioner_Decommission_NoOpForUnknownTenant(t *testing.T) {
	// Decommission for a tenant that was never provisioned is a no-op so the
	// admin-repair flow can re-issue Decommission without checking state.
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	require.NoError(t, p.Decommission(context.Background(), uuid.New()),
		"Decommission must be idempotent / no-op for unknown tenants")
}

func TestLocalBucketProvisioner_Concurrent(t *testing.T) {
	// Sanity: concurrent Provision calls for the same tenant must not race
	// nor double-create a bucket. The Local provisioner is the dev fallback
	// but the contract is the same as the Yandex impl.
	t.Parallel()

	p := store.NewLocalBucketProvisioner("sociopulse-recordings-")
	tenantID := uuid.New()
	const goroutines = 16

	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	names := make(chan string, goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			name, err := p.Provision(context.Background(), tenantID, "kek-id-1")
			if err != nil {
				errs <- err
				return
			}
			names <- name
		}()
	}
	wg.Wait()
	close(errs)
	close(names)

	for err := range errs {
		require.NoError(t, err)
	}

	first := <-names
	for n := range names {
		require.Equal(t, first, n, "every concurrent caller must observe the same bucket name")
	}
	require.Equal(t, 1, p.Count(), "concurrent Provision must NOT create more than one bucket")
}

func TestLocalBucketProvisioner_ImplementsAPIInterface(t *testing.T) {
	t.Parallel()
	// Compile-time-style check: NewLocalBucketProvisioner must satisfy
	// api.BucketProvisioner. The compile-time assertion lives next to the
	// struct; this test confirms the constructor itself returns a value
	// that can be assigned to the interface.
	var _ api.BucketProvisioner = store.NewLocalBucketProvisioner("sociopulse-recordings-")
}

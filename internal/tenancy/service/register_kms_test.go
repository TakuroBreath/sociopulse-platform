package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/service"
	_ "github.com/sociopulse/platform/internal/tenancy/service" // init() seam
	"github.com/sociopulse/platform/internal/tenancy/store"
)

// TestKMSResolverImpl_AssignableToAPI proves the concrete service.KMSResolverImpl
// is assignable to the api.KMSResolver interface used by other modules.
func TestKMSResolverImpl_AssignableToAPI(t *testing.T) {
	t.Parallel()

	rs := &resolverStore{}
	rs.getFn = func(_ context.Context, _ uuid.UUID) (api.Tenant, error) {
		return api.Tenant{}, api.ErrNotFound
	}
	kms := newFakeKMSClient()

	resolver := service.NewKMSResolver(zaptest.NewLogger(t), rs, kms, service.KMSResolverConfig{})
	t.Cleanup(resolver.Close)
	var _ api.KMSResolver = resolver
	require.NotNil(t, resolver)
}

// TestKMSResolverImpl_BuiltAgainstLocalKMSClient verifies that the
// local in-process KMS client (the dev fallback) plugs into the
// resolver and round-trips a payload end-to-end. This is the path
// `make dev-up` exercises.
func TestKMSResolverImpl_BuiltAgainstLocalKMSClient(t *testing.T) {
	t.Parallel()

	masterKey := strings.Repeat("ab", 32) // 32 bytes hex-encoded
	kmsClient, err := store.NewLocalKMSClient(masterKey)
	require.NoError(t, err)

	// Pre-create a KEK for the synthetic tenant.
	keyID, err := kmsClient.CreateKey(context.Background(), "tenant-X", "")
	require.NoError(t, err)

	tenantID := uuid.New()
	rs := &resolverStore{}
	rs.getFn = func(_ context.Context, id uuid.UUID) (api.Tenant, error) {
		require.Equal(t, tenantID, id)
		return api.Tenant{ID: tenantID, KMSKEKID: keyID}, nil
	}

	resolver := service.NewKMSResolver(zaptest.NewLogger(t), rs, kmsClient, service.KMSResolverConfig{})
	t.Cleanup(resolver.Close)

	plaintext := []byte("the quick brown fox jumps over the lazy dog")
	ct, err := resolver.Encrypt(context.Background(), tenantID, plaintext)
	require.NoError(t, err)
	require.NotEqual(t, plaintext, ct)

	pt, err := resolver.Decrypt(context.Background(), tenantID, ct)
	require.NoError(t, err)
	require.Equal(t, plaintext, pt)
}

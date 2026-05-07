package service_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/service"
)

// fakeKMSClientForResolver is a hand-rolled api.KMSClient double that
// behaves like a real envelope-style KMS for the resolver tests:
//   - CreateKey allocates a fresh 32-byte symmetric KEK per name.
//   - Encrypt/Decrypt are deterministic (XOR-with-KEK plus a 4-byte tag)
//     so the resolver can be exercised without real crypto.
//   - GenerateDataKey returns a fresh 32-byte plaintext + its wrapping.
//
// XOR-with-KEK is NOT real cryptography — the goal here is to exercise
// the resolver's caching/error/branch logic, NOT the security of the
// envelope. The local KMS client (kms_client_local_test.go) covers real
// AES-256-GCM behaviour.
type fakeKMSClientForResolver struct {
	mu sync.Mutex

	keys map[string][]byte // keyID -> 32-byte KEK plaintext

	encryptCalls atomic.Int32
	decryptCalls atomic.Int32
	gendkCalls   atomic.Int32

	encryptErr error
	decryptErr error
	gendkErr   error

	// nextDEK is yielded by GenerateDataKey if non-nil; otherwise a
	// random-looking 32-byte slice is returned.
	nextDEK []byte
}

func newFakeKMSClient() *fakeKMSClientForResolver {
	return &fakeKMSClientForResolver{
		keys: make(map[string][]byte),
	}
}

// putKey lets tests pre-register a KEK without going through CreateKey.
func (f *fakeKMSClientForResolver) putKey(id string, kek []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[id] = kek
}

func (f *fakeKMSClientForResolver) CreateKey(_ context.Context, name, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "kek-" + name
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + len(name))
	}
	f.keys[id] = kek
	return id, nil
}

func (f *fakeKMSClientForResolver) Encrypt(_ context.Context, keyID string, plaintext []byte) ([]byte, string, error) {
	f.encryptCalls.Add(1)
	if f.encryptErr != nil {
		return nil, "", f.encryptErr
	}
	f.mu.Lock()
	kek, ok := f.keys[keyID]
	f.mu.Unlock()
	if !ok {
		return nil, "", errors.New("fake kms: unknown keyID")
	}
	out := xorMask(plaintext, kek)
	return out, "v1-" + keyID, nil
}

func (f *fakeKMSClientForResolver) Decrypt(_ context.Context, keyID string, ciphertext []byte) ([]byte, string, error) {
	f.decryptCalls.Add(1)
	if f.decryptErr != nil {
		return nil, "", f.decryptErr
	}
	f.mu.Lock()
	kek, ok := f.keys[keyID]
	f.mu.Unlock()
	if !ok {
		return nil, "", errors.New("fake kms: unknown keyID")
	}
	out := xorMask(ciphertext, kek)
	return out, "v1-" + keyID, nil
}

func (f *fakeKMSClientForResolver) GenerateDataKey(ctx context.Context, keyID string) ([]byte, []byte, string, error) {
	f.gendkCalls.Add(1)
	if f.gendkErr != nil {
		return nil, nil, "", f.gendkErr
	}
	f.mu.Lock()
	kek, ok := f.keys[keyID]
	dek := f.nextDEK
	f.mu.Unlock()
	if !ok {
		return nil, nil, "", errors.New("fake kms: unknown keyID")
	}
	if dek == nil {
		dek = make([]byte, 32)
		for i := range dek {
			dek[i] = byte((i * 7) ^ 0xA5)
		}
	}
	wrapped := xorMask(dek, kek)
	_ = ctx
	return dek, wrapped, "v1-" + keyID, nil
}

// xorMask is the deterministic stand-in for KMS encryption used by the
// resolver tests. It cycles `mask` over `data` and returns a new slice.
func xorMask(data, mask []byte) []byte {
	out := make([]byte, len(data))
	for i := range data {
		out[i] = data[i] ^ mask[i%len(mask)]
	}
	return out
}

// resolverStore is a minimal api.Store double that only implements Get —
// the only method the resolver touches. Other methods are unused; if the
// resolver starts calling them, the test surfaces it as an unexpected
// call.
type resolverStore struct {
	fakeStore // embed the fakeStore from tenant_service_test.go
}

func newResolverStore(t *testing.T, tenant api.Tenant) *resolverStore {
	t.Helper()
	rs := &resolverStore{}
	rs.getFn = func(_ context.Context, id uuid.UUID) (api.Tenant, error) {
		require.Equal(t, tenant.ID, id)
		return tenant, nil
	}
	return rs
}

func newResolverStoreNotFound(_ *testing.T) *resolverStore {
	rs := &resolverStore{}
	rs.getFn = func(_ context.Context, _ uuid.UUID) (api.Tenant, error) {
		return api.Tenant{}, api.ErrNotFound
	}
	return rs
}

func TestKMSResolver_EnsureKEK_DelegatesToStore(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: "kek-tenant-foo"}

	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), newFakeKMSClient(), service.KMSResolverConfig{})

	got, err := r.EnsureKEK(context.Background(), tenantID)
	require.NoError(t, err)
	require.Equal(t, "kek-tenant-foo", got)
}

func TestKMSResolver_EnsureKEK_PropagatesNotFound(t *testing.T) {
	t.Parallel()

	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStoreNotFound(t), newFakeKMSClient(), service.KMSResolverConfig{})

	_, err := r.EnsureKEK(context.Background(), uuid.New())
	require.ErrorIs(t, err, api.ErrNotFound)
}

func TestKMSResolver_EnsureKEK_RejectsTenantWithoutKEK(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: ""} // never been provisioned

	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), newFakeKMSClient(), service.KMSResolverConfig{})

	_, err := r.EnsureKEK(context.Background(), tenantID)
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestKMSResolver_GenerateDataKey_PassesThroughToKMS(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: "kek-1"}

	kms := newFakeKMSClient()
	kms.putKey("kek-1", []byte("00000000000000000000000000000000"))

	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})

	dk, err := r.GenerateDataKey(context.Background(), tenantID)
	require.NoError(t, err)
	require.Len(t, dk.Plaintext, 32, "DEK plaintext must be 32 bytes")
	require.NotEmpty(t, dk.Ciphertext)
	require.Equal(t, "v1-kek-1", dk.KeyVersion)
	require.Equal(t, int32(1), kms.gendkCalls.Load())
}

func TestKMSResolver_GenerateDataKey_PropagatesKMSError(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: "kek-1"}

	kms := newFakeKMSClient()
	kms.putKey("kek-1", []byte("00000000000000000000000000000000"))
	kms.gendkErr = errors.New("yandex kms transient")

	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})

	_, err := r.GenerateDataKey(context.Background(), tenantID)
	require.ErrorIs(t, err, api.ErrKMSUnavailable,
		"GenerateDataKey must surface api.ErrKMSUnavailable on transient errors")
}

func TestKMSResolver_EncryptDecrypt_Roundtrip(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: "kek-1"}

	kms := newFakeKMSClient()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	kms.putKey("kek-1", kek)

	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})

	plaintext := []byte("super-secret-payload-for-tenant")
	ct, err := r.Encrypt(context.Background(), tenantID, plaintext)
	require.NoError(t, err)
	require.NotEmpty(t, ct)
	require.NotEqual(t, plaintext, ct)

	pt, err := r.Decrypt(context.Background(), tenantID, ct)
	require.NoError(t, err)
	require.Equal(t, plaintext, pt)
}

func TestKMSResolver_Encrypt_CachesDEK_AvoidsSecondGenerateDataKey(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: "kek-1"}

	kms := newFakeKMSClient()
	kek := make([]byte, 32)
	kms.putKey("kek-1", kek)

	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})

	for i := 0; i < 5; i++ {
		_, err := r.Encrypt(context.Background(), tenantID, []byte("payload"))
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), kms.gendkCalls.Load(),
		"DEK cache must reuse the first GenerateDataKey across subsequent Encrypts")
}

func TestKMSResolver_Decrypt_LazyUnwrapOnCacheMiss(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: "kek-1"}

	kms := newFakeKMSClient()
	kek := make([]byte, 32)
	kms.putKey("kek-1", kek)

	// Encrypt with one resolver instance to produce ciphertext; then
	// decrypt with a fresh resolver instance whose cache is cold.
	r1 := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})
	ct, err := r1.Encrypt(context.Background(), tenantID, []byte("payload"))
	require.NoError(t, err)

	r2 := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})

	pt, err := r2.Decrypt(context.Background(), tenantID, ct)
	require.NoError(t, err)
	require.Equal(t, []byte("payload"), pt)

	require.GreaterOrEqual(t, kms.decryptCalls.Load(), int32(1),
		"a cold-cache Decrypt MUST call kms.Decrypt at least once to unwrap the DEK")
}

func TestKMSResolver_Decrypt_KMSDecryptErrorMapsToUnavailable(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: "kek-1"}

	kms := newFakeKMSClient()
	kek := make([]byte, 32)
	kms.putKey("kek-1", kek)

	// Encrypt happily, then break Decrypt before unwrapping.
	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})
	ct, err := r.Encrypt(context.Background(), tenantID, []byte("payload"))
	require.NoError(t, err)
	r.InvalidateCache(tenantID) // force the next Decrypt to call kms.Decrypt

	kms.decryptErr = errors.New("yandex kms transient")
	_, err = r.Decrypt(context.Background(), tenantID, ct)
	require.ErrorIs(t, err, api.ErrKMSUnavailable)
}

func TestKMSResolver_Decrypt_RejectsMalformedCiphertext(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: "kek-1"}

	kms := newFakeKMSClient()
	kms.putKey("kek-1", make([]byte, 32))

	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})

	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"three bytes", []byte{1, 2, 3}},
		{"length prefix overshoot", []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00}},
	}
	for _, tc := range cases {
		_, err := r.Decrypt(context.Background(), tenantID, tc.in)
		require.ErrorIs(t, err, api.ErrInvalidArgument,
			"case %q: expected ErrInvalidArgument", tc.name)
	}
}

func TestKMSResolver_DifferentTenantsHaveSeparateCacheEntries(t *testing.T) {
	t.Parallel()

	idA, idB := uuid.New(), uuid.New()
	tenantA := api.Tenant{ID: idA, KMSKEKID: "kek-A"}
	tenantB := api.Tenant{ID: idB, KMSKEKID: "kek-B"}

	kms := newFakeKMSClient()
	kekA := make([]byte, 32)
	kekB := make([]byte, 32)
	for i := range kekA {
		kekA[i] = 0x11
		kekB[i] = 0x22
	}
	kms.putKey("kek-A", kekA)
	kms.putKey("kek-B", kekB)

	rs := &resolverStore{}
	rs.getFn = func(_ context.Context, id uuid.UUID) (api.Tenant, error) {
		switch id {
		case idA:
			return tenantA, nil
		case idB:
			return tenantB, nil
		}
		return api.Tenant{}, api.ErrNotFound
	}

	r := service.NewKMSResolver(zaptest.NewLogger(t), rs, kms, service.KMSResolverConfig{})

	ctA, err := r.Encrypt(context.Background(), idA, []byte("A's payload"))
	require.NoError(t, err)
	ctB, err := r.Encrypt(context.Background(), idB, []byte("B's payload"))
	require.NoError(t, err)

	require.GreaterOrEqual(t, kms.gendkCalls.Load(), int32(2),
		"each tenant must trigger its own GenerateDataKey")

	// Decrypts must round-trip per tenant; cross-tenant decrypt with the
	// wrong cache must NOT succeed (the wrapped DEK in ctA names kek-A,
	// not kek-B).
	ptA, err := r.Decrypt(context.Background(), idA, ctA)
	require.NoError(t, err)
	require.Equal(t, []byte("A's payload"), ptA)

	ptB, err := r.Decrypt(context.Background(), idB, ctB)
	require.NoError(t, err)
	require.Equal(t, []byte("B's payload"), ptB)
}

func TestKMSResolver_InvalidateCache_ForcesNextEncryptToCallKMS(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: "kek-1"}

	kms := newFakeKMSClient()
	kms.putKey("kek-1", make([]byte, 32))

	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})

	_, err := r.Encrypt(context.Background(), tenantID, []byte("a"))
	require.NoError(t, err)
	require.Equal(t, int32(1), kms.gendkCalls.Load())

	r.InvalidateCache(tenantID)

	_, err = r.Encrypt(context.Background(), tenantID, []byte("b"))
	require.NoError(t, err)
	require.Equal(t, int32(2), kms.gendkCalls.Load(),
		"after InvalidateCache, the next Encrypt must mint a fresh DEK from KMS")
}

func TestKMSResolver_ImplementsAPIInterface(t *testing.T) {
	t.Parallel()
	var _ api.KMSResolver = service.NewKMSResolver(
		zaptest.NewLogger(t), &resolverStore{}, newFakeKMSClient(), service.KMSResolverConfig{})
}

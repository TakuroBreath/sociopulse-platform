package service_test

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/service"
	"github.com/sociopulse/platform/internal/tenancy/store"
	"github.com/sociopulse/platform/pkg/encryption"
)

// genHexKeyAAD returns 32 random bytes encoded as a hex string. The local
// KMS dev fallback expects exactly that input format. (Package-local
// clone of the helper in pkg store_test — test files don't cross-import.)
func genHexKeyAAD(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return hex.EncodeToString(buf)
}

// realKMSAndKEK constructs a real (local) KMS client and provisions a
// KEK for the supplied tenant ID. Returns the client and the kekID.
// Tests in this file use realKMSAndKEK so AAD binding is faithfully
// exercised end to end (the XOR fake in kms_resolver_test.go does NOT
// honour AAD).
func realKMSAndKEK(t *testing.T, tenantID uuid.UUID) (api.KMSClient, string) {
	t.Helper()
	kms, err := store.NewLocalKMSClient(genHexKeyAAD(t))
	require.NoError(t, err)
	kekID, err := kms.CreateKey(t.Context(), tenantID.String(), "test")
	require.NoError(t, err)
	return kms, kekID
}

// realResolver wires the local KMS through the production resolver.
// The store is hard-coded to return the supplied (tenantID, kekID) pair
// — adequate for single-tenant cases. For multi-tenant cases use a
// dedicated resolver per tenant or build the store inline.
func realResolver(t *testing.T, tenantID uuid.UUID, kekID string, kms api.KMSClient) *service.KMSResolverImpl {
	t.Helper()
	tenant := api.Tenant{ID: tenantID, KMSKEKID: kekID}
	r := service.NewKMSResolver(zaptest.NewLogger(t),
		newResolverStore(t, tenant), kms, service.KMSResolverConfig{})
	t.Cleanup(r.Close)
	return r
}

// TestKMSResolver_Encrypt_WritesV2Prefix asserts the resolver prepends
// 0x02 as the first byte of every new ciphertext. This is the on-disk
// marker that lets a later Decrypt know "AAD-bound, use BuildAAD" vs.
// "legacy, AAD=nil".
func TestKMSResolver_Encrypt_WritesV2Prefix(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	kms, kekID := realKMSAndKEK(t, tenantID)
	r := realResolver(t, tenantID, kekID, kms)

	ct, err := r.Encrypt(t.Context(), tenantID, "auth.user.phone", uuid.NewString(), []byte("payload"))
	require.NoError(t, err)
	require.NotEmpty(t, ct)
	require.Equal(t, byte(0x02), ct[0],
		"resolver Encrypt must prepend 0x02 version byte (v2 = BuildAAD-bound)")
}

// TestKMSResolver_DecryptV2_Roundtrip is the happy-path equivalent of
// the existing EncryptDecrypt_Roundtrip test, but uses the real local
// KMS so the AES-GCM Open call truly evaluates the AAD parameter.
// Round-trip with matching scope+rowID must succeed.
func TestKMSResolver_DecryptV2_Roundtrip(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	kms, kekID := realKMSAndKEK(t, tenantID)
	r := realResolver(t, tenantID, kekID, kms)

	const scope = "auth.user.phone"
	rowID := uuid.NewString()
	plain := []byte("+79991234567")

	ct, err := r.Encrypt(t.Context(), tenantID, scope, rowID, plain)
	require.NoError(t, err)

	got, err := r.Decrypt(t.Context(), tenantID, scope, rowID, ct)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}

// TestKMSResolver_DecryptV2_RejectsTenantSwap exercises the
// cross-tenant ciphertext-swap attack: an attacker who places Tenant A's
// ciphertext into a row belonging to Tenant B fails AEAD auth tag
// verification. This is the headline reason BuildAAD exists.
//
// Two tenants, two KEKs, one shared resolver. Encrypt under Tenant A
// and attempt decrypt as Tenant B with the SAME row id and scope.
// The local KMS already rejects ciphertext from the wrong KEK at the
// wrapping layer; with BuildAAD the column AEAD tag is ALSO a gate —
// defence in depth. This test documents the high-level invariant.
func TestKMSResolver_DecryptV2_RejectsTenantSwap(t *testing.T) {
	t.Parallel()

	idA := uuid.New()
	idB := uuid.New()

	kms, err := store.NewLocalKMSClient(genHexKeyAAD(t))
	require.NoError(t, err)
	kekA, err := kms.CreateKey(t.Context(), idA.String(), "test")
	require.NoError(t, err)
	kekB, err := kms.CreateKey(t.Context(), idB.String(), "test")
	require.NoError(t, err)

	tenantA := api.Tenant{ID: idA, KMSKEKID: kekA}
	tenantB := api.Tenant{ID: idB, KMSKEKID: kekB}

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
	t.Cleanup(r.Close)

	const scope = "auth.user.phone"
	rowID := uuid.NewString()
	ct, err := r.Encrypt(t.Context(), idA, scope, rowID, []byte("A's phone"))
	require.NoError(t, err)

	_, err = r.Decrypt(t.Context(), idB, scope, rowID, ct)
	require.Error(t, err,
		"decrypt under Tenant B of Tenant A's ciphertext must fail")
}

// TestKMSResolver_DecryptV2_RejectsScopeSwap is the most surgical AAD
// test: encrypt as scope X, attempt decrypt as scope Y under the SAME
// tenant and same rowID. The wrapped-DEK matches, so the only thing
// blocking decryption is the AAD difference. This catches a bug where
// the resolver builds AAD but accidentally passes nil to Open.
func TestKMSResolver_DecryptV2_RejectsScopeSwap(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	kms, kekID := realKMSAndKEK(t, tenantID)
	r := realResolver(t, tenantID, kekID, kms)

	rowID := uuid.NewString()
	ct, err := r.Encrypt(t.Context(), tenantID, "auth.user.phone", rowID, []byte("secret"))
	require.NoError(t, err)

	_, err = r.Decrypt(t.Context(), tenantID, "auth.totp.secret", rowID, ct)
	require.Error(t, err, "scope-swap decrypt MUST fail at AEAD auth-tag")
	require.ErrorIs(t, err, api.ErrInvalidArgument,
		"resolver surfaces AEAD-auth failures as ErrInvalidArgument")
}

// TestKMSResolver_DecryptV2_RejectsRowSwap is the headline finding:
// an attacker swaps the phone ciphertext from user A's row into user B's
// row. Same tenant, same scope — only the row ID changes. Without the
// row ID in AAD, this attack succeeds at the AEAD layer.
//
// This is the precise security defect Task 6 closes.
func TestKMSResolver_DecryptV2_RejectsRowSwap(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	kms, kekID := realKMSAndKEK(t, tenantID)
	r := realResolver(t, tenantID, kekID, kms)

	const scope = "auth.user.phone"
	rowA := uuid.NewString()
	rowB := uuid.NewString()
	ct, err := r.Encrypt(t.Context(), tenantID, scope, rowA, []byte("A's phone"))
	require.NoError(t, err)

	_, err = r.Decrypt(t.Context(), tenantID, scope, rowB, ct)
	require.Error(t, err,
		"row-swap (A's ciphertext, B's rowID) MUST fail at AEAD auth-tag")
	require.ErrorIs(t, err, api.ErrInvalidArgument,
		"resolver surfaces AEAD-auth failures as ErrInvalidArgument")
}

// TestKMSResolver_DecryptV1_LegacyUnprefixed_Roundtrip asserts backward
// compatibility with existing production ciphertexts. The plan ships
// v2 ciphertexts with a 0x02 version-byte prefix; production data
// written prior to this plan has no version byte and starts with 0x00
// (the high byte of the wrapped-DEK length prefix). The resolver MUST
// keep decrypting those old blobs without AAD bind.
func TestKMSResolver_DecryptV1_LegacyUnprefixed_Roundtrip(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	kms, kekID := realKMSAndKEK(t, tenantID)
	r := realResolver(t, tenantID, kekID, kms)

	// Build a legacy v1 envelope:
	//   [4-byte BE wrapped-DEK length][wrapped-DEK][AES-GCM blob with AAD=nil]
	plain := []byte("legacy-payload")

	dekPlain, wrappedDEK, _, err := kms.GenerateDataKey(t.Context(), kekID)
	require.NoError(t, err)

	body, err := encryption.Encrypt(dekPlain, plain, nil)
	require.NoError(t, err)

	legacyCT := make([]byte, 0, 4+len(wrappedDEK)+len(body))
	legacyCT = binary.BigEndian.AppendUint32(legacyCT, uint32(len(wrappedDEK))) //nolint:gosec // bounded
	legacyCT = append(legacyCT, wrappedDEK...)
	legacyCT = append(legacyCT, body...)

	// Sanity: the legacy envelope starts with 0x00 (high byte of length
	// prefix), distinguishing it from a v2 envelope's leading 0x02.
	require.Equal(t, byte(0x00), legacyCT[0],
		"legacy envelope must start with 0x00 (length-prefix high byte)")

	// Plan 13.2.5 Task 6: scope/rowID are ignored on the legacy decrypt
	// path; the resolver detects "no version byte" and uses nil AAD.
	got, err := r.Decrypt(t.Context(), tenantID, "auth.user.phone", uuid.NewString(), legacyCT)
	require.NoError(t, err, "legacy unprefixed ciphertext must decrypt unchanged")
	require.Equal(t, plain, got)
}

// TestKMSResolver_DecryptV1_VersionedLegacy_Roundtrip exercises the
// explicit-versioned legacy path: a ciphertext prefixed with 0x01
// (the spec's "legacy" version byte) is decoded without AAD bind.
// This handles a future re-write of production data through a
// "stamp every row with 0x01" migration.
func TestKMSResolver_DecryptV1_VersionedLegacy_Roundtrip(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	kms, kekID := realKMSAndKEK(t, tenantID)
	r := realResolver(t, tenantID, kekID, kms)

	// Build a 0x01-prefixed legacy envelope:
	//   [0x01][4-byte BE wrapped-DEK length][wrapped-DEK][AES-GCM blob with AAD=nil]
	plain := []byte("versioned-legacy-payload")
	dekPlain, wrappedDEK, _, err := kms.GenerateDataKey(t.Context(), kekID)
	require.NoError(t, err)
	body, err := encryption.Encrypt(dekPlain, plain, nil)
	require.NoError(t, err)

	legacyCT := make([]byte, 0, 1+4+len(wrappedDEK)+len(body))
	legacyCT = append(legacyCT, 0x01)
	legacyCT = binary.BigEndian.AppendUint32(legacyCT, uint32(len(wrappedDEK))) //nolint:gosec // bounded
	legacyCT = append(legacyCT, wrappedDEK...)
	legacyCT = append(legacyCT, body...)

	got, err := r.Decrypt(t.Context(), tenantID, "auth.user.phone", uuid.NewString(), legacyCT)
	require.NoError(t, err, "0x01-prefixed legacy ciphertext must decrypt unchanged")
	require.Equal(t, plain, got)
}

// TestKMSResolver_DecryptUnknownVersion_ReturnsSentinel asserts the
// resolver fails closed on a version byte it doesn't recognise. This
// shields the system from a future spec change being smuggled in via
// crafted ciphertext.
func TestKMSResolver_DecryptUnknownVersion_ReturnsSentinel(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	kms, kekID := realKMSAndKEK(t, tenantID)
	r := realResolver(t, tenantID, kekID, kms)

	// 0x05 is reserved for future use; any byte not in {0x00, 0x01, 0x02}
	// must be rejected.
	garbage := []byte{0x05, 0x00, 0x00, 0x00, 0x00, 0x00}
	_, err := r.Decrypt(t.Context(), tenantID, "auth.user.phone", uuid.NewString(), garbage)
	require.Error(t, err)
	require.ErrorIs(t, err, api.ErrInvalidArgument,
		"unknown ciphertext version must surface ErrInvalidArgument")
}

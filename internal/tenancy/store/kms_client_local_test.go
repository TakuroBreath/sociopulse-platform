package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/store"
)

// genHexKey returns 32 random bytes encoded as a hex string. The local KMS
// dev fallback expects exactly that input format.
func genHexKey(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return hex.EncodeToString(buf)
}

func TestNewLocalKMSClient_RejectsEmptyKey(t *testing.T) {
	t.Parallel()
	_, err := store.NewLocalKMSClient("")
	require.Error(t, err)
}

func TestNewLocalKMSClient_RejectsBadHex(t *testing.T) {
	t.Parallel()
	_, err := store.NewLocalKMSClient("zz-not-hex")
	require.Error(t, err)
}

func TestNewLocalKMSClient_RejectsWrongKeyLength(t *testing.T) {
	t.Parallel()
	// 16 bytes is too short — AES-256 needs 32.
	_, err := store.NewLocalKMSClient(strings.Repeat("00", 16))
	require.Error(t, err)
}

func TestLocalKMSClient_CreateKey_ReturnsStableID(t *testing.T) {
	t.Parallel()
	c, err := store.NewLocalKMSClient(genHexKey(t))
	require.NoError(t, err)

	id1, err := c.CreateKey(context.Background(), "tenant-foo", "desc")
	require.NoError(t, err)
	require.NotEmpty(t, id1)

	id2, err := c.CreateKey(context.Background(), "tenant-bar", "desc")
	require.NoError(t, err)
	require.NotEqual(t, id1, id2, "different names must produce different IDs")
}

func TestLocalKMSClient_EncryptDecryptRoundtrip(t *testing.T) {
	t.Parallel()
	c, err := store.NewLocalKMSClient(genHexKey(t))
	require.NoError(t, err)

	keyID, err := c.CreateKey(context.Background(), "tenant-foo", "desc")
	require.NoError(t, err)

	plaintext := []byte("hello-secret-payload")
	ct, ver, err := c.Encrypt(context.Background(), keyID, plaintext)
	require.NoError(t, err)
	require.NotEqual(t, plaintext, ct, "ciphertext must differ from plaintext")
	require.NotEmpty(t, ver, "version id must be non-empty")

	pt, ver2, err := c.Decrypt(context.Background(), keyID, ct)
	require.NoError(t, err)
	require.Equal(t, plaintext, pt)
	require.Equal(t, ver, ver2, "decrypted version must match encrypted version")
}

func TestLocalKMSClient_RejectsCiphertextFromDifferentKey(t *testing.T) {
	t.Parallel()
	c, err := store.NewLocalKMSClient(genHexKey(t))
	require.NoError(t, err)

	keyA, err := c.CreateKey(context.Background(), "tenant-A", "")
	require.NoError(t, err)
	keyB, err := c.CreateKey(context.Background(), "tenant-B", "")
	require.NoError(t, err)

	ct, _, err := c.Encrypt(context.Background(), keyA, []byte("payload"))
	require.NoError(t, err)

	_, _, err = c.Decrypt(context.Background(), keyB, ct)
	require.Error(t, err, "decryption with the wrong KEK must fail")
}

func TestLocalKMSClient_RejectsDecryptForUnknownKey(t *testing.T) {
	t.Parallel()
	c, err := store.NewLocalKMSClient(genHexKey(t))
	require.NoError(t, err)

	_, _, err = c.Decrypt(context.Background(), "nonexistent-key-id", []byte("garbage"))
	require.ErrorIs(t, err, api.ErrKEKNotFound,
		"unknown KEK must surface api.ErrKEKNotFound, got %v", err)
}

func TestLocalKMSClient_GenerateDataKey_Roundtrip(t *testing.T) {
	t.Parallel()
	c, err := store.NewLocalKMSClient(genHexKey(t))
	require.NoError(t, err)

	keyID, err := c.CreateKey(context.Background(), "tenant-foo", "")
	require.NoError(t, err)

	pt, ct, ver, err := c.GenerateDataKey(context.Background(), keyID)
	require.NoError(t, err)
	require.Len(t, pt, 32, "DEK plaintext must be 32 bytes for AES-256")
	require.NotEmpty(t, ct)
	require.NotEmpty(t, ver)

	// The wrapped DEK should round-trip via Decrypt.
	pt2, _, err := c.Decrypt(context.Background(), keyID, ct)
	require.NoError(t, err)
	require.Equal(t, pt, pt2, "GenerateDataKey + Decrypt must yield the original DEK")
}

func TestLocalKMSClient_ImplementsAPIInterface(t *testing.T) {
	t.Parallel()
	// Compile-time-style check.
	c, err := store.NewLocalKMSClient(genHexKey(t))
	require.NoError(t, err)
	var _ api.KMSClient = c
}

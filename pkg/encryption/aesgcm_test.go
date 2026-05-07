package encryption_test

import (
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/encryption"
)

// key32 returns 32 random bytes for AES-256 key tests.
func key32(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	t.Parallel()

	k := key32(t)
	plaintext := []byte("+79161234567")

	ct, err := encryption.Encrypt(k, plaintext, nil)
	require.NoError(t, err)
	require.NotEqual(t, plaintext, ct, "ciphertext must differ from plaintext")
	// nonce(12) + plaintext + tag(16)
	require.GreaterOrEqual(t, len(ct), 12+len(plaintext)+16, "must include nonce+plaintext+tag")

	got, err := encryption.Decrypt(k, ct, nil)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

func TestEncrypt_NonceIsRandomEachCall(t *testing.T) {
	t.Parallel()

	k := key32(t)
	plaintext := []byte("hello")

	a, err := encryption.Encrypt(k, plaintext, nil)
	require.NoError(t, err)
	b, err := encryption.Encrypt(k, plaintext, nil)
	require.NoError(t, err)

	require.NotEqual(t, a, b, "two encrypts of the same plaintext must yield different ciphertext")
}

func TestDecrypt_RejectsTamperedCiphertext(t *testing.T) {
	t.Parallel()

	k := key32(t)
	ct, err := encryption.Encrypt(k, []byte("important"), nil)
	require.NoError(t, err)

	// Flip a bit somewhere in the body (after the nonce).
	ct[15] ^= 0x01

	_, err = encryption.Decrypt(k, ct, nil)
	require.ErrorIs(t, err, encryption.ErrAuth, "must surface ErrAuth on MAC failure")
}

func TestDecrypt_RejectsMismatchedAAD(t *testing.T) {
	t.Parallel()

	k := key32(t)
	tenant := []byte("tenant-A")
	other := []byte("tenant-B")

	ct, err := encryption.Encrypt(k, []byte("payload"), tenant)
	require.NoError(t, err)

	_, err = encryption.Decrypt(k, ct, other)
	require.ErrorIs(t, err, encryption.ErrAuth)

	got, err := encryption.Decrypt(k, ct, tenant)
	require.NoError(t, err)
	require.Equal(t, []byte("payload"), got)
}

func TestEncrypt_RejectsBadKeyLength(t *testing.T) {
	t.Parallel()

	_, err := encryption.Encrypt(make([]byte, 16), []byte("x"), nil)
	require.ErrorIs(t, err, encryption.ErrKeySize)
}

func TestDecrypt_RejectsShortCiphertext(t *testing.T) {
	t.Parallel()

	k := key32(t)
	_, err := encryption.Decrypt(k, []byte("x"), nil)
	require.Error(t, err)
}

func TestDecrypt_RejectsBadKeyLength(t *testing.T) {
	t.Parallel()

	// Build a valid ciphertext with one key, then try to open with a wrong-sized key.
	k := key32(t)
	ct, err := encryption.Encrypt(k, []byte("x"), nil)
	require.NoError(t, err)

	_, err = encryption.Decrypt(make([]byte, 16), ct, nil)
	require.ErrorIs(t, err, encryption.ErrKeySize)
}

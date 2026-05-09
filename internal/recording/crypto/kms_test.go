package crypto_test

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/pkg/encryption"
)

func TestLocalDEKUnwrapper_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)

	dekPlain := make([]byte, 32)
	_, err = rand.Read(dekPlain)
	require.NoError(t, err)

	aad := []byte("tenant-A")
	encryptedDEK, err := encryption.Encrypt(kek, dekPlain, aad)
	require.NoError(t, err)

	u := crypto.NewLocalDEKUnwrapper(map[string][]byte{"kek-tenant-A": kek})
	got, err := u.DecryptDEK(ctx, "kek-tenant-A", encryptedDEK, aad)
	require.NoError(t, err)
	require.Equal(t, dekPlain, got)
}

func TestLocalDEKUnwrapper_UnknownKey(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	u := crypto.NewLocalDEKUnwrapper(map[string][]byte{}) // empty
	_, err := u.DecryptDEK(ctx, "kek-tenant-A", []byte("anything"), nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, crypto.ErrUnknownKey),
		"expected ErrUnknownKey, got %v", err)
}

func TestLocalDEKUnwrapper_AADMismatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	encryptedDEK, err := encryption.Encrypt(kek, []byte("dek-payload"), []byte("tenant-A"))
	require.NoError(t, err)

	u := crypto.NewLocalDEKUnwrapper(map[string][]byte{"kek-1": kek})
	_, err = u.DecryptDEK(ctx, "kek-1", encryptedDEK, []byte("tenant-B"))
	require.Error(t, err)
	require.True(t, errors.Is(err, crypto.ErrDecryptFailed),
		"expected ErrDecryptFailed on AAD mismatch, got %v", err)
	// Lock in the spec scrub contract — like Task 1's ErrAuth fold.
	require.False(t, errors.Is(err, encryption.ErrAuth),
		"encryption.ErrAuth must not leak through the fold")
	require.NotContains(t, err.Error(), "cipher",
		"underlying cipher error text must not appear in the message; got %q", err.Error())
}

func TestLocalDEKUnwrapper_CtxCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	kek := make([]byte, 32)
	u := crypto.NewLocalDEKUnwrapper(map[string][]byte{"kek-1": kek})
	_, err := u.DecryptDEK(ctx, "kek-1", []byte("anything"), nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled),
		"expected context.Canceled, got %v", err)
}

func TestNewLocalDEKUnwrapper_ClonesInputMap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	keks := map[string][]byte{"kek-1": kek}

	u := crypto.NewLocalDEKUnwrapper(keks)

	// Mutate the original map AND the slice — the unwrapper must be
	// unaffected because the constructor clones.
	keks["kek-1"] = nil
	delete(keks, "kek-1")
	for i := range kek {
		kek[i] = 0
	}

	// Round-trip with a fresh encrypt under the original kek bytes.
	freshKEK := make([]byte, 32)
	_, _ = rand.Read(freshKEK)
	keks2 := map[string][]byte{"kek-2": freshKEK}
	u2 := crypto.NewLocalDEKUnwrapper(keks2)

	encryptedDEK, err := encryption.Encrypt(freshKEK, []byte("dek"), nil)
	require.NoError(t, err)
	got, err := u2.DecryptDEK(ctx, "kek-2", encryptedDEK, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("dek"), got)

	// And the FIRST unwrapper should still know "kek-1" exists, despite
	// the caller having deleted it from the original map.
	_, err = u.DecryptDEK(ctx, "kek-1", []byte("anything"), nil)
	// We expect ErrDecryptFailed (not ErrUnknownKey) — the key entry
	// is still registered, but the ciphertext "anything" is bogus.
	require.True(t, errors.Is(err, crypto.ErrDecryptFailed),
		"key must remain registered despite caller mutations; got %v", err)
}

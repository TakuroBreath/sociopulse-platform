package crypto_test

import (
	"context"
	"crypto/rand"
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
	require.ErrorIs(t, err, crypto.ErrUnknownKey)
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
	require.ErrorIs(t, err, crypto.ErrDecryptFailed)
	// Lock in the spec scrub contract — like Task 1's ErrAuth fold.
	require.NotErrorIs(t, err, encryption.ErrAuth,
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
	require.ErrorIs(t, err, context.Canceled)
}

func TestNewLocalDEKUnwrapper_ClonesInputMap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Generate a KEK and encrypt a payload under it BEFORE handing it
	// to the constructor. Then mutate (zero) the original KEK bytes and
	// delete the map entry. If the constructor cloned both the map and
	// each value, the unwrapper still owns the original KEK bytes and
	// can successfully decrypt the payload.
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)

	dekPayload := []byte("dek-payload-for-clone-test")
	encryptedDEK, err := encryption.Encrypt(kek, dekPayload, nil)
	require.NoError(t, err)

	keks := map[string][]byte{"kek-1": kek}
	u := crypto.NewLocalDEKUnwrapper(keks)

	// Now sabotage the caller-side state: zero the KEK bytes AND the map
	// entry. Without the per-value clone, the unwrapper's decrypt would
	// run AES-256-GCM with an all-zero key against the (originally valid)
	// ciphertext — the auth tag fails and we'd get ErrDecryptFailed.
	for i := range kek {
		kek[i] = 0
	}
	keks["kek-1"] = nil
	delete(keks, "kek-1")

	// The decrypt MUST succeed — the unwrapper owns its own copy of the
	// pre-zero KEK bytes, and "kek-1" is still registered because the
	// constructor cloned the map shell too.
	got, err := u.DecryptDEK(ctx, "kek-1", encryptedDEK, nil)
	require.NoError(t, err, "constructor must defensively clone both the map and each KEK byte slice")
	require.Equal(t, dekPayload, got)
}

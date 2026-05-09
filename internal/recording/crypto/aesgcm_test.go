package crypto_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/pkg/encryption"
)

func TestAESGCMDecryptor_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	plaintext := []byte("hello recording audio")
	aad := []byte("tenant-A")

	ct, err := encryption.Encrypt(key, plaintext, aad)
	require.NoError(t, err)

	d := crypto.NewAESGCMDecryptor()
	got, err := d.Decrypt(ctx, key, bytes.NewReader(ct), int64(len(ct)), aad)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

func TestAESGCMDecryptor_AADMismatchYieldsErrAuth(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	ct, err := encryption.Encrypt(key, []byte("audio"), []byte("tenant-A"))
	require.NoError(t, err)

	d := crypto.NewAESGCMDecryptor()
	_, err = d.Decrypt(ctx, key, bytes.NewReader(ct), int64(len(ct)), []byte("tenant-B"))
	require.Error(t, err)
	require.True(t, errors.Is(err, crypto.ErrAuth), "expected ErrAuth, got %v", err)

	// Locks in the spec's "never expose tampering details" contract:
	// the inner encryption.ErrAuth and crypto/cipher's text MUST NOT
	// be reachable through the returned error chain. Plan 12.3's HTTP
	// handler relies on this scrub when mapping ErrAuth → 502.
	require.False(t, errors.Is(err, encryption.ErrAuth),
		"encryption.ErrAuth must not leak through the fold")
	require.NotContains(t, err.Error(), "cipher",
		"underlying cipher error text must not appear in the message; got %q", err.Error())
}

func TestAESGCMDecryptor_TamperedCiphertextYieldsErrAuth(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	ct, err := encryption.Encrypt(key, []byte("audio"), nil)
	require.NoError(t, err)
	ct[len(ct)-1] ^= 0xFF // flip last byte of tag

	d := crypto.NewAESGCMDecryptor()
	_, err = d.Decrypt(ctx, key, bytes.NewReader(ct), int64(len(ct)), nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, crypto.ErrAuth))
}

func TestAESGCMDecryptor_SizeCapRejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	d := crypto.NewAESGCMDecryptor()
	_, err := d.Decrypt(ctx, make([]byte, 32), bytes.NewReader(nil), crypto.MaxAudioPlaintextBytes+1, nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, crypto.ErrAudioTooLargeForV1))
}

func TestAESGCMDecryptor_InvalidSize(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	d := crypto.NewAESGCMDecryptor()
	_, err := d.Decrypt(ctx, make([]byte, 32), bytes.NewReader(nil), 0, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid size")

	_, err = d.Decrypt(ctx, make([]byte, 32), bytes.NewReader(nil), -1, nil)
	require.Error(t, err)
}

func TestAESGCMDecryptor_CtxCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	ct, _ := encryption.Encrypt(key, []byte("audio"), nil)

	d := crypto.NewAESGCMDecryptor()
	// OneByteReader forces multiple reads so the ctx check fires
	_, err := d.Decrypt(ctx, key, iotest.OneByteReader(bytes.NewReader(ct)), int64(len(ct)), nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
}

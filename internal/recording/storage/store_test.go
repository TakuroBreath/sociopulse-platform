package storage_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/storage"
)

func TestLocalObjectStore_PutThenGetReturnsBytes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := storage.NewLocalObjectStore()
	payload := []byte("opus.enc audio bytes")
	s.PutBytes("bucket-A", "recordings/x/y/z.opus.enc", payload)

	rc, err := s.Get(ctx, "bucket-A", "recordings/x/y/z.opus.enc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Close() })

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestLocalObjectStore_GetMissingReturnsNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := storage.NewLocalObjectStore()
	_, err := s.Get(ctx, "bucket-A", "missing-key")
	require.Error(t, err)
	require.True(t, errors.Is(err, storage.ErrObjectNotFound),
		"expected ErrObjectNotFound, got %v", err)
}

func TestLocalObjectStore_DeleteMissingIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := storage.NewLocalObjectStore()
	err := s.Delete(ctx, "bucket-A", "never-existed")
	require.NoError(t, err)
}

func TestLocalObjectStore_DeleteThenGetReturnsNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := storage.NewLocalObjectStore()
	s.PutBytes("bucket-A", "k", []byte("payload"))

	require.NoError(t, s.Delete(ctx, "bucket-A", "k"))
	_, err := s.Get(ctx, "bucket-A", "k")
	require.True(t, errors.Is(err, storage.ErrObjectNotFound))
}

func TestLocalObjectStore_GetIsolatedFromCallerMutations(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := storage.NewLocalObjectStore()
	s.PutBytes("bucket-A", "k", []byte{0x01, 0x02, 0x03})

	rc, err := s.Get(ctx, "bucket-A", "k")
	require.NoError(t, err)

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	require.Equal(t, []byte{0x01, 0x02, 0x03}, got)

	// Mutate the returned buffer — must NOT affect a subsequent Get.
	for i := range got {
		got[i] = 0xFF
	}

	rc2, err := s.Get(ctx, "bucket-A", "k")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc2.Close() })
	got2, err := io.ReadAll(rc2)
	require.NoError(t, err)
	require.Equal(t, []byte{0x01, 0x02, 0x03}, got2,
		"caller mutations must not corrupt the store")
}

func TestLocalObjectStore_PutIsolatedFromCallerMutations(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s := storage.NewLocalObjectStore()
	payload := []byte{0x01, 0x02, 0x03}
	s.PutBytes("bucket-A", "k", payload)

	// Mutate the input buffer AFTER PutBytes — must NOT affect what's stored.
	for i := range payload {
		payload[i] = 0xFF
	}

	rc, err := s.Get(ctx, "bucket-A", "k")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Close() })
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, []byte{0x01, 0x02, 0x03}, got,
		"PutBytes must clone the input — caller mutations after Put must not reach the store")
}

func TestLocalObjectStore_ConcurrentPutAndGet(t *testing.T) {
	t.Parallel()

	s := storage.NewLocalObjectStore()
	const goroutines = 10
	const iterations = 1000

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range iterations {
				key := keyFor(g, i)
				payload := []byte{byte(g), byte(i)}
				s.PutBytes("bucket-A", key, payload)
				rc, err := s.Get(context.Background(), "bucket-A", key)
				require.NoError(t, err)
				_, err = io.ReadAll(rc)
				require.NoError(t, err)
				_ = rc.Close()
			}
		}(g)
	}
	wg.Wait()
}

func TestLocalObjectStore_CtxCancelled(t *testing.T) {
	t.Parallel()

	s := storage.NewLocalObjectStore()
	s.PutBytes("bucket-A", "k", []byte("payload"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Get(ctx, "bucket-A", "k")
	require.True(t, errors.Is(err, context.Canceled))

	err = s.Delete(ctx, "bucket-A", "k")
	require.True(t, errors.Is(err, context.Canceled))
}

// keyFor produces a deterministic unique key per (goroutine, iteration).
func keyFor(g, i int) string {
	return "concurrent/" + itoa(g) + "/" + itoa(i)
}

// itoa avoids strconv import for a tiny test helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := make([]byte, 0, 12)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	// reverse
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

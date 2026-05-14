package storage_test

import (
	"context"
	"io"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
	require.ErrorIs(t, err, storage.ErrObjectNotFound)
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
	require.ErrorIs(t, err, storage.ErrObjectNotFound)
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

	// Use assert (not require) inside the goroutine: testifylint flags
	// require.* in non-test goroutines because t.FailNow only stops the
	// goroutine that called it, leaving the WaitGroup hanging on a panic.
	// assert.NoError logs the failure on t and lets the loop continue;
	// wg.Wait still completes and the parent test sees the assertion fail.
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
				assert.NoError(t, err)
				_, err = io.ReadAll(rc)
				assert.NoError(t, err)
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
	require.ErrorIs(t, err, context.Canceled)

	err = s.Delete(ctx, "bucket-A", "k")
	require.ErrorIs(t, err, context.Canceled)
}

func TestLocalObjectStore_Put_RoundTrip(t *testing.T) {
	t.Parallel()
	s := storage.NewLocalObjectStore()
	ctx := context.Background()

	payload := []byte("hello world")
	require.NoError(t, s.Put(ctx, "test-bucket", "test/key", payload, "text/plain"))

	rc, err := s.Get(ctx, "test-bucket", "test/key")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestLocalObjectStore_Put_OverwritesExisting(t *testing.T) {
	t.Parallel()
	s := storage.NewLocalObjectStore()
	ctx := context.Background()
	require.NoError(t, s.Put(ctx, "b", "k", []byte("v1"), "text/plain"))
	require.NoError(t, s.Put(ctx, "b", "k", []byte("v2"), "text/plain"))
	rc, err := s.Get(ctx, "b", "k")
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	require.Equal(t, []byte("v2"), got)
}

func TestLocalObjectStore_Put_CtxCanceled(t *testing.T) {
	t.Parallel()
	s := storage.NewLocalObjectStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Put(ctx, "b", "k", []byte("v"), "text/plain")
	require.ErrorIs(t, err, context.Canceled)
}

func TestLocalObjectStore_PresignedURL_Shape(t *testing.T) {
	t.Parallel()
	s := storage.NewLocalObjectStore()
	ctx := context.Background()
	u, err := s.PresignedURL(ctx, "bucket-x", "key/sub", 24*time.Hour)
	require.NoError(t, err)
	parsed, err := url.Parse(u)
	require.NoError(t, err)
	require.Equal(t, "local", parsed.Scheme)
	require.Equal(t, "bucket-x", parsed.Host)
	require.Equal(t, "/key/sub", parsed.Path)
	require.NotEmpty(t, parsed.Query().Get("expires"))
}

func TestLocalObjectStore_PresignedURL_RejectsNonPositiveTTL(t *testing.T) {
	t.Parallel()
	s := storage.NewLocalObjectStore()
	_, err := s.PresignedURL(context.Background(), "b", "k", 0)
	require.Error(t, err)
	_, err = s.PresignedURL(context.Background(), "b", "k", -time.Second)
	require.Error(t, err)
}

func TestLocalObjectStore_PresignedURL_CtxCanceled(t *testing.T) {
	t.Parallel()
	s := storage.NewLocalObjectStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.PresignedURL(ctx, "b", "k", time.Hour)
	require.ErrorIs(t, err, context.Canceled)
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

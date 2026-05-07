package service_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/service"
)

// newPepperFakeStore returns a fakeStore (from tenant_service_test.go) whose
// GetPhoneHashPepper hook captures call count and delegates to fn. Re-using
// the in-package fakeStore keeps the doubles strategy uniform across the
// service test binary.
func newPepperFakeStore(
	fn func(ctx context.Context, tenantID uuid.UUID) ([]byte, error),
) (*fakeStore, *atomic.Int64) {
	calls := &atomic.Int64{}
	fs := &fakeStore{
		getPepperFn: func(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
			calls.Add(1)
			return fn(ctx, tenantID)
		},
	}
	return fs, calls
}

// uniformPepper returns a 32-byte pepper filled with the given byte.
func uniformPepper(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestPhoneHasher_NormaliseHappyPath(t *testing.T) {
	t.Parallel()

	h := service.NewPhoneHasher(zaptest.NewLogger(t), nil, service.PhoneHasherConfig{})
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plus_e164_ru_with_spaces", "+7 999 123-45-67", "+79991234567"},
		{"plus_e164_ru_compact", "+79991234567", "+79991234567"},
		{"local_8_prefix", "8 (999) 123-45-67", "+79991234567"},
		{"plus_e164_uk", "+44 20 7946 0958", "+442079460958"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := h.Normalise(c.in)
			require.NoError(t, err, c.in)
			require.Equal(t, c.want, got, c.in)
		})
	}
}

func TestPhoneHasher_NormaliseRejectsGarbage(t *testing.T) {
	t.Parallel()

	h := service.NewPhoneHasher(zaptest.NewLogger(t), nil, service.PhoneHasherConfig{})
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"alpha", "abc"},
		{"too_short", "1234"},
		{"missing_plus_short", "+1"},
		{"too_long", "+99999999999999999"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Normalise(c.in)
			require.Error(t, err, c.in)
			require.ErrorIs(t, err, api.ErrInvalidArgument, c.in)
		})
	}
}

func TestPhoneHasher_HashIsDeterministicPerTenant(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	pepper := make([]byte, 32)
	for i := range pepper {
		pepper[i] = byte(i)
	}
	store, _ := newPepperFakeStore(func(_ context.Context, _ uuid.UUID) ([]byte, error) {
		return append([]byte(nil), pepper...), nil
	})

	h := service.NewPhoneHasher(zaptest.NewLogger(t), store, service.PhoneHasherConfig{})
	h1, err := h.Hash(context.Background(), tenantID, "+79991234567")
	require.NoError(t, err)
	require.Len(t, h1, 32)

	h2, err := h.Hash(context.Background(), tenantID, "8 (999) 123-45-67")
	require.NoError(t, err)
	require.Equal(t, h1, h2, "the same E.164 normalisation must yield the same hash")
}

func TestPhoneHasher_HashesDifferAcrossTenants(t *testing.T) {
	t.Parallel()

	a := uuid.New()
	b := uuid.New()
	store, _ := newPepperFakeStore(func(_ context.Context, tenantID uuid.UUID) ([]byte, error) {
		switch tenantID {
		case a:
			return uniformPepper(0x00), nil
		case b:
			return uniformPepper(0xFF), nil
		}
		return nil, api.ErrNotFound
	})

	h := service.NewPhoneHasher(zaptest.NewLogger(t), store, service.PhoneHasherConfig{})
	ha, err := h.Hash(context.Background(), a, "+79991234567")
	require.NoError(t, err)
	hb, err := h.Hash(context.Background(), b, "+79991234567")
	require.NoError(t, err)
	require.NotEqual(t, ha, hb,
		"different peppers must yield different hashes for the same phone")
}

func TestPhoneHasher_HashRejectsInvalidPhone(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	store, _ := newPepperFakeStore(func(_ context.Context, _ uuid.UUID) ([]byte, error) {
		return uniformPepper(0x42), nil
	})
	h := service.NewPhoneHasher(zaptest.NewLogger(t), store, service.PhoneHasherConfig{})

	_, err := h.Hash(context.Background(), tenantID, "garbage")
	require.Error(t, err)
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestPhoneHasher_HashSurfacesPepperLookupErrors(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	store, _ := newPepperFakeStore(func(_ context.Context, _ uuid.UUID) ([]byte, error) {
		return nil, api.ErrNotFound
	})
	h := service.NewPhoneHasher(zaptest.NewLogger(t), store, service.PhoneHasherConfig{})

	_, err := h.Hash(context.Background(), tenantID, "+79991234567")
	require.Error(t, err)
	require.ErrorIs(t, err, api.ErrNotFound, "missing tenant must propagate api.ErrNotFound")
}

func TestPhoneHasher_HashRejectsShortPepper(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	store, _ := newPepperFakeStore(func(_ context.Context, _ uuid.UUID) ([]byte, error) {
		return make([]byte, 16), nil // intentionally too short
	})
	h := service.NewPhoneHasher(zaptest.NewLogger(t), store, service.PhoneHasherConfig{})

	_, err := h.Hash(context.Background(), tenantID, "+79991234567")
	require.Error(t, err)
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestPhoneHasher_PepperCacheReusesLookup(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	store, calls := newPepperFakeStore(func(_ context.Context, _ uuid.UUID) ([]byte, error) {
		return uniformPepper(0x42), nil
	})
	h := service.NewPhoneHasher(zaptest.NewLogger(t), store, service.PhoneHasherConfig{
		PepperCacheTTL:  5 * time.Minute,
		PepperCacheSize: 16,
	})

	for i := 0; i < 8; i++ {
		_, err := h.Hash(context.Background(), tenantID, "+79991234567")
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), calls.Load(),
		"pepper cache must coalesce repeated lookups; got %d store calls", calls.Load())
}

// TestPhoneHasher_PepperCacheTTLForcesReFetch ensures stale cache entries are
// re-resolved from the store after TTL elapses. The clock indirection lets
// the test fast-forward without sleeping.
func TestPhoneHasher_PepperCacheTTLForcesReFetch(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	store, calls := newPepperFakeStore(func(_ context.Context, _ uuid.UUID) ([]byte, error) {
		return uniformPepper(0x42), nil
	})

	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	h := service.NewPhoneHasherWithClock(zaptest.NewLogger(t), store, service.PhoneHasherConfig{
		PepperCacheTTL:  100 * time.Millisecond,
		PepperCacheSize: 16,
	}, clock)

	_, err := h.Hash(context.Background(), tenantID, "+79991234567")
	require.NoError(t, err)
	require.Equal(t, int64(1), calls.Load())

	// Advance well beyond the TTL.
	now = now.Add(time.Second)

	_, err = h.Hash(context.Background(), tenantID, "+79991234567")
	require.NoError(t, err)
	require.Equal(t, int64(2), calls.Load(),
		"expired cache entry must be re-fetched; got %d store calls", calls.Load())
}

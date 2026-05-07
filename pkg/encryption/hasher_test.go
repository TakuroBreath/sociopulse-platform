package encryption_test

import (
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/encryption"
)

func TestPhoneHasher_Deterministic(t *testing.T) {
	t.Parallel()

	pepper := make([]byte, 32)
	_, err := rand.Read(pepper)
	require.NoError(t, err)

	h := encryption.NewPhoneHasher(pepper)

	a := h.Hash("+79161234567")
	b := h.Hash("+79161234567")
	require.Equal(t, a, b, "same input must produce same hash")
}

func TestPhoneHasher_DifferentPeppersProduceDifferentHashes(t *testing.T) {
	t.Parallel()

	p1 := make([]byte, 32)
	p2 := make([]byte, 32)
	for i := range p1 {
		p1[i] = byte(i)
		p2[i] = byte(255 - i)
	}

	// Same pepper instance must hash identical inputs identically — this
	// is just a determinism guard; the main check is that p1 vs p2 differ.
	hash1A := encryption.NewPhoneHasher(p1).Hash("+79161234567")
	hash1B := encryption.NewPhoneHasher(p1).Hash("+79161234567")
	require.Equal(t, hash1A, hash1B, "p1 must be deterministic across hasher instances")

	hash2 := encryption.NewPhoneHasher(p2).Hash("+79161234567")
	require.NotEqual(t, hash1A, hash2, "different peppers must yield different hashes")
}

func TestPhoneHasher_NormalizesInputs(t *testing.T) {
	t.Parallel()

	pepper := make([]byte, 32)
	for i := range pepper {
		pepper[i] = 0x42
	}
	h := encryption.NewPhoneHasher(pepper)

	want := h.Hash("+79161234567")
	require.Equal(t, want, h.Hash("89161234567"))
	require.Equal(t, want, h.Hash("+7 (916) 123-45-67"))
	require.Equal(t, want, h.Hash("7-916-123-45-67"))
}

func TestPhoneHasher_OutputLength(t *testing.T) {
	t.Parallel()

	pepper := make([]byte, 32)
	h := encryption.NewPhoneHasher(pepper)
	require.Len(t, h.Hash("+79161234567"), 32, "HMAC-SHA256 must be 32 bytes")
}

func TestPhoneHasher_RejectsShortPepper(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		encryption.NewPhoneHasher(make([]byte, 16))
	})
}

func TestPhoneHasher_DifferentPhonesDifferentHash(t *testing.T) {
	t.Parallel()

	pepper := make([]byte, 32)
	for i := range pepper {
		pepper[i] = byte(i)
	}
	h := encryption.NewPhoneHasher(pepper)
	require.NotEqual(t, h.Hash("+79161234567"), h.Hash("+79161234568"))
}

func TestPhoneHasher_PepperCopyIsolatesCallerMutation(t *testing.T) {
	t.Parallel()

	// NewPhoneHasher must defensively copy the pepper, so that callers
	// who reuse / zero / mutate their slice don't compromise the hasher.
	pepper := make([]byte, 32)
	for i := range pepper {
		pepper[i] = 0x11
	}
	h := encryption.NewPhoneHasher(pepper)
	want := h.Hash("+79161234567")

	// Wipe the caller's pepper slice.
	for i := range pepper {
		pepper[i] = 0
	}

	require.Equal(t, want, h.Hash("+79161234567"),
		"hasher must be isolated from caller's pepper buffer")
}

func TestNormalizePhone_Russian11With8(t *testing.T) {
	t.Parallel()
	require.Equal(t, "+79161234567", encryption.NormalizePhone("89161234567"))
}

func TestNormalizePhone_Russian11With7(t *testing.T) {
	t.Parallel()
	require.Equal(t, "+79161234567", encryption.NormalizePhone("79161234567"))
}

func TestNormalizePhone_Russian10NoPrefix(t *testing.T) {
	t.Parallel()
	require.Equal(t, "+79161234567", encryption.NormalizePhone("9161234567"))
}

func TestNormalizePhone_StripsFormatting(t *testing.T) {
	t.Parallel()
	require.Equal(t, "+79161234567", encryption.NormalizePhone("+7 (916) 123-45-67"))
}

func TestNormalizePhone_PassesThroughUnknown(t *testing.T) {
	t.Parallel()
	// Non-Russian / unrecognised pattern: pass through unchanged so the
	// import pipeline can detect and reject downstream.
	require.Equal(t, "abc", encryption.NormalizePhone("abc"))
}

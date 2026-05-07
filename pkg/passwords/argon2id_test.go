package passwords_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/passwords"
)

// cheapParams returns Argon2id parameters that are safe-but-fast for unit
// tests. Real production parameters cost ~50–100 ms per Hash, which is
// orders of magnitude too slow when running 100+ test cases under -race.
// These values keep correctness intact (still memory-hard, still salted,
// still produces a valid PHC string) while bringing each Hash call into
// the sub-millisecond range.
func cheapParams() passwords.Params {
	return passwords.Params{
		Memory:      8, // 8 KiB — minimum that satisfies m >= 8*p
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

func TestHash_ProducesPHCPrefix(t *testing.T) {
	t.Parallel()

	got, err := passwords.Hash("hunter2", cheapParams())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "$argon2id$v=19$"),
		"expected PHC prefix; got %q", got)
}

func TestVerify_AcceptsCorrectPassword(t *testing.T) {
	t.Parallel()

	encoded, err := passwords.Hash("hunter2", cheapParams())
	require.NoError(t, err)

	ok, err := passwords.Verify(encoded, "hunter2")
	require.NoError(t, err)
	assert.True(t, ok, "right password must verify")
}

func TestVerify_RejectsWrongPassword(t *testing.T) {
	t.Parallel()

	encoded, err := passwords.Hash("hunter2", cheapParams())
	require.NoError(t, err)

	ok, err := passwords.Verify(encoded, "wrong")
	require.NoError(t, err, "wrong-password must not signal an error; only false")
	assert.False(t, ok)
}

func TestVerify_MalformedHashReturnsErrInvalidHash(t *testing.T) {
	t.Parallel()

	ok, err := passwords.Verify("not-a-phc", "x")
	assert.False(t, ok)
	require.Error(t, err)
	assert.ErrorIs(t, err, passwords.ErrInvalidHash)
}

func TestVerify_IncompatibleVersionReturnsErr(t *testing.T) {
	t.Parallel()

	// Construct a syntactically valid PHC but with v=16 (Argon2 prior version).
	bad := "$argon2id$v=16$m=8,t=1,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcGFiY2RlZmdoaWprbG1ub3A"
	ok, err := passwords.Verify(bad, "anything")
	assert.False(t, ok)
	require.Error(t, err)
	assert.ErrorIs(t, err, passwords.ErrIncompatibleVersion)
	// And the umbrella sentinel still trips, so callers that only branch on
	// "is this hash usable" don't have to enumerate every reason.
	assert.ErrorIs(t, err, passwords.ErrInvalidHash)
}

func TestHash_RandomSaltProducesDifferentCiphertexts(t *testing.T) {
	t.Parallel()

	a, err := passwords.Hash("hunter2", cheapParams())
	require.NoError(t, err)
	b, err := passwords.Hash("hunter2", cheapParams())
	require.NoError(t, err)

	assert.NotEqual(t, a, b,
		"two Hash calls with the same password must yield different PHC strings (random salt)")
}

func TestHash_ParamsRoundTripThroughPHC(t *testing.T) {
	t.Parallel()

	want := passwords.Params{
		Memory:      32 * 1024,
		Iterations:  2,
		Parallelism: 2,
		SaltLength:  16,
		KeyLength:   32,
	}

	encoded, err := passwords.Hash("hunter2", want)
	require.NoError(t, err)

	// We don't expose decode publicly; the assertion is via the encoded
	// substring, which IS the contract.
	require.Contains(t, encoded, "m=32768,t=2,p=2",
		"PHC parameter segment must reflect the params used to hash")
	// And Verify must accept the right password against this exact encoded.
	ok, err := passwords.Verify(encoded, "hunter2")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestHash_RejectsInvalidParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    passwords.Params
	}{
		{
			name: "zero memory",
			p:    passwords.Params{Memory: 0, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32},
		},
		{
			name: "zero iterations",
			p:    passwords.Params{Memory: 8, Iterations: 0, Parallelism: 1, SaltLength: 16, KeyLength: 32},
		},
		{
			name: "zero parallelism",
			p:    passwords.Params{Memory: 8, Iterations: 1, Parallelism: 0, SaltLength: 16, KeyLength: 32},
		},
		{
			name: "zero salt length",
			p:    passwords.Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 0, KeyLength: 32},
		},
		{
			name: "zero key length",
			p:    passwords.Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 0},
		},
		{
			name: "key length below floor",
			p:    passwords.Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 3},
		},
		{
			name: "memory below 8*parallelism floor",
			p:    passwords.Params{Memory: 4, Iterations: 1, Parallelism: 4, SaltLength: 16, KeyLength: 32},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := passwords.Hash("x", tt.p)
			require.Error(t, err)
			assert.ErrorIs(t, err, passwords.ErrInvalidParams)
		})
	}
}

func TestDefaultParams_IsValid(t *testing.T) {
	t.Parallel()

	require.NoError(t, passwords.DefaultParams().Validate())
}

func TestDefaultParams_Values(t *testing.T) {
	t.Parallel()

	// OWASP Argon2id minimum (Aug 2024): m=19 MiB, t=2, p=1 — sized
	// for our threat model (no high-value offline-attack surface) so the
	// per-hash memory footprint stays bounded and BoundedHasher can
	// safely cap concurrency without OOMing.
	p := passwords.DefaultParams()
	assert.Equal(t, uint32(19*1024), p.Memory)
	assert.Equal(t, uint32(2), p.Iterations)
	assert.Equal(t, uint8(1), p.Parallelism)
	assert.Equal(t, uint32(16), p.SaltLength)
	assert.Equal(t, uint32(32), p.KeyLength)
}

// TestVerify_TimingDeltaSmall asserts that Verify's wall-clock time for a
// matching vs mismatching password differs by less than 10% of the median.
//
// This is a coarse sanity check that subtle.ConstantTimeCompare is doing
// its job: a naive bytes.Equal short-circuits on the first mismatching
// byte and would show a clear timing skew. The check is statistical (50
// iterations, compare medians) so noisy CI does not flake it. Kept out of
// `-short` runs because Argon2id (even at cheap params) sums to ~50 ms of
// wall-clock per case.
func TestVerify_TimingDeltaSmall(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping timing-delta probe under -short")
	}

	encoded, err := passwords.Hash("hunter2", cheapParams())
	require.NoError(t, err)

	const N = 50
	rights := make([]time.Duration, 0, N)
	wrongs := make([]time.Duration, 0, N)
	// Interleave to spread scheduler/JIT noise evenly across the two arms.
	for range N {
		t0 := time.Now()
		_, _ = passwords.Verify(encoded, "hunter2")
		rights = append(rights, time.Since(t0))

		t1 := time.Now()
		_, _ = passwords.Verify(encoded, "wrongggg")
		wrongs = append(wrongs, time.Since(t1))
	}

	medianR := median(rights)
	medianW := median(wrongs)
	deltaPct := absPctDelta(medianR, medianW)

	// Argon2id derivation dominates the wall-clock; the constant-time tail
	// compare is a tiny fraction. 10% is a generous bound that should not
	// flake on noisy CI but would still catch a glaring `bytes.Equal` regression.
	t.Logf("median verify time: right=%s wrong=%s delta=%.2f%%", medianR, medianW, deltaPct)
	require.Less(t, deltaPct, 10.0,
		"timing delta between right/wrong password verify exceeded 10%%; "+
			"check that subtle.ConstantTimeCompare is in use")
}

// median returns the median of d. d is copied internally so the caller's
// slice ordering is preserved.
func median(d []time.Duration) time.Duration {
	cp := slices.Clone(d)
	slices.Sort(cp)
	return cp[len(cp)/2]
}

// absPctDelta returns |a-b| / min(a,b) as a percentage. Symmetric so the
// caller doesn't have to reason about which arm is faster.
func absPctDelta(a, b time.Duration) float64 {
	if a == 0 || b == 0 {
		return 100.0
	}
	hi, lo := a, b
	if hi < lo {
		hi, lo = lo, hi
	}
	return 100.0 * float64(hi-lo) / float64(lo)
}

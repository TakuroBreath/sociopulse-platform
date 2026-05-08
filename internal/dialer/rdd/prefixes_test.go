package rdd

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/regions"
)

// fixedRng returns a *rand.Rand seeded with a deterministic ChaCha8
// state. Tests that rely on specific prefix selections call this so
// the assertions are reproducible across runs.
func fixedRng(t *testing.T) *rand.Rand {
	t.Helper()
	//nolint:gosec // non-crypto: deterministic test seed.
	return rand.New(rand.NewChaCha8([32]byte{
		'r', 'd', 'd', '-', 't', 'e', 's', 't',
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	}))
}

// TestPickPrefixForRegion_HappyPath confirms a prefix is returned from
// the region's published list, never from anywhere else.
func TestPickPrefixForRegion_HappyPath(t *testing.T) {
	t.Parallel()
	r := regions.Region{
		Code:        "RU-MOW",
		DEFPrefixes: []string{"915", "916", "917"},
	}
	rng := fixedRng(t)
	for range 100 {
		prefix, err := pickPrefixForRegion(rng, r, false)
		require.NoError(t, err)
		require.Contains(t, r.DEFPrefixes, prefix)
	}
}

// TestPickPrefixForRegion_EmptyPrefixes returns the sentinel error so
// callers can bucket the attempt as InvalidHit.
func TestPickPrefixForRegion_EmptyPrefixes(t *testing.T) {
	t.Parallel()
	r := regions.Region{Code: "RU-EMPTY", DEFPrefixes: nil}
	_, err := pickPrefixForRegion(fixedRng(t), r, false)
	require.ErrorIs(t, err, errNoEligiblePrefix)
}

// TestRollSubscriber_LengthAndDigits — every roll yields exactly
// subscriberDigits decimal digits.
func TestRollSubscriber_LengthAndDigits(t *testing.T) {
	t.Parallel()
	rng := fixedRng(t)
	for range 100 {
		s := rollSubscriber(rng)
		require.Len(t, s, subscriberDigits)
		for _, ch := range s {
			require.True(t, ch >= '0' && ch <= '9', "subscriber must be all digits, got %q", s)
		}
	}
}

// TestComposePhone shapes the canonical E.164.
func TestComposePhone(t *testing.T) {
	t.Parallel()
	got := composePhone("916", "1234567")
	require.Equal(t, "+79161234567", got)
}

// TestValidE164RU — accept canonical RU mobile, reject the documented
// failure modes.
func TestValidE164RU(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		phone string
		ok    bool
	}{
		{"canonical Moscow Beeline", "+79161234567", true},
		{"canonical SPb MTS", "+79111234567", true},
		{"missing plus", "79161234567", false},
		{"wrong country code", "+19161234567", false},
		{"too short", "+7916123456", false},
		{"too long", "+791612345678", false},
		{"non-digit body", "+7916abc4567", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, c.ok, validE164RU(c.phone), "phone %q", c.phone)
		})
	}
}

// TestRollSubscriber_DistributionSanity — over a large sample, every
// digit position covers the full [0,9] range. Cheap insurance against
// a future bug like "always rolling a 0" (e.g. an off-by-one in
// IntN(10) that returned IntN(1)).
func TestRollSubscriber_DistributionSanity(t *testing.T) {
	t.Parallel()
	rng := fixedRng(t)
	const samples = 10_000
	seen := [subscriberDigits]map[byte]bool{}
	for i := range subscriberDigits {
		seen[i] = make(map[byte]bool)
	}
	for range samples {
		s := rollSubscriber(rng)
		for i := range subscriberDigits {
			seen[i][s[i]] = true
		}
	}
	for i, m := range seen {
		require.Len(t, m, 10, "position %d must cover all 10 digits across %d samples", i, samples)
	}
}

// TestComposePhone_AlwaysValid — composing any rolled subscriber with
// any region prefix yields a phone that passes validE164RU.
func TestComposePhone_AlwaysValid(t *testing.T) {
	t.Parallel()
	r := regions.Region{Code: "RU-MOW", DEFPrefixes: []string{"915", "916", "917"}}
	rng := fixedRng(t)
	for range 100 {
		prefix, err := pickPrefixForRegion(rng, r, false)
		require.NoError(t, err)
		sub := rollSubscriber(rng)
		phone := composePhone(prefix, sub)
		require.True(t, validE164RU(phone), "composed phone %q must pass validation", phone)
		require.True(t, strings.HasPrefix(phone, "+7"+prefix))
	}
}

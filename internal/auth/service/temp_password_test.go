package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateTempPassword_ProducesExpectedShape(t *testing.T) {
	t.Parallel()

	pwd, err := GenerateTempPassword()
	require.NoError(t, err)
	require.Len(t, pwd, 16)
}

func TestGenerateTempPassword_OnlyURLSafeChars(t *testing.T) {
	t.Parallel()

	const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	pwd, err := GenerateTempPassword()
	require.NoError(t, err)
	for i, ch := range pwd {
		require.True(t,
			strings.ContainsRune(allowed, ch),
			"char %d (%q) is outside the URL-safe alphabet", i, ch,
		)
	}
}

func TestGenerateTempPassword_NoDuplicatesAcrossManyCalls(t *testing.T) {
	t.Parallel()

	const iterations = 1000
	seen := make(map[string]struct{}, iterations)
	for i := 0; i < iterations; i++ {
		pwd, err := GenerateTempPassword()
		require.NoError(t, err)
		_, dup := seen[pwd]
		require.False(t, dup, "iteration %d produced a duplicate password", i)
		seen[pwd] = struct{}{}
	}
}

func TestGenerateTempPassword_UsesEnoughOfTheAlphabet(t *testing.T) {
	t.Parallel()

	// Probabilistic sanity: across 1000 calls × 16 chars = 16000 emissions
	// from a 64-char alphabet, we expect virtually every char to appear.
	// We require at least 50/64 distinct chars — the chance of seeing
	// fewer due to randomness alone is negligible (<<1e-9).
	seen := make(map[byte]struct{}, 64)
	for i := 0; i < 1000; i++ {
		pwd, err := GenerateTempPassword()
		require.NoError(t, err)
		for j := 0; j < len(pwd); j++ {
			seen[pwd[j]] = struct{}{}
		}
	}
	require.GreaterOrEqual(t, len(seen), 50,
		"only %d distinct chars seen across 16000 emissions; alphabet may be biased",
		len(seen),
	)
}

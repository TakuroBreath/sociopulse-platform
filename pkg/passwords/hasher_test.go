package passwords_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/passwords"
)

func TestDefault_HashVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Default() uses production Argon2 parameters which are slow under -short")
	}

	ctx := context.Background()
	h := passwords.Default()

	encoded, err := h.Hash(ctx, "hunter2")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(encoded, "$argon2id$v=19$"),
		"Default().Hash must produce a PHC string; got %q", encoded)

	ok, err := h.Verify(ctx, encoded, "hunter2")
	require.NoError(t, err)
	assert.True(t, ok, "Default().Verify must accept the matching password")

	bad, err := h.Verify(ctx, encoded, "wrong")
	require.NoError(t, err)
	assert.False(t, bad, "Default().Verify must reject a wrong password")
}

func TestNewHasher_HonoursCustomParams(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	// Cheap parameters keep the test fast; using NewHasher is the documented
	// way for callers to opt into non-defaults.
	h := passwords.NewHasher(passwords.Params{
		Memory:      8,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	})

	encoded, err := h.Hash(ctx, "hunter2")
	require.NoError(t, err)
	require.Contains(t, encoded, "m=8,t=1,p=1",
		"PHC params segment must reflect the params passed to NewHasher")

	ok, err := h.Verify(ctx, encoded, "hunter2")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestNewHasher_PropagatesParamValidation(t *testing.T) {
	t.Parallel()

	// Zero Memory is invalid; the error should surface from Hash, wrapped
	// through ErrInvalidParams.
	h := passwords.NewHasher(passwords.Params{
		Memory:      0,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	})
	_, err := h.Hash(context.Background(), "x")
	require.Error(t, err)
	assert.ErrorIs(t, err, passwords.ErrInvalidParams)
}

func TestNewHasher_VerifyForwardsMalformed(t *testing.T) {
	t.Parallel()

	// Verify ignores the constructor's Params and reads them from the
	// encoded string; a malformed encoded must surface ErrInvalidHash even
	// when the hasher itself was constructed with valid params.
	h := passwords.NewHasher(passwords.Params{
		Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	})

	ok, err := h.Verify(context.Background(), "definitely-not-a-phc", "x")
	assert.False(t, ok)
	assert.ErrorIs(t, err, passwords.ErrInvalidHash)
}

// Compile-time check from the test side that Default() returns a Hasher.
// If the interface ever drifts this line refuses to compile.
var _ passwords.Hasher = passwords.Default()

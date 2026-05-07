package passwords

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encoding_test.go covers the package-private encode/decode helpers.
// We live in the internal test package (same package) so we can call them
// without exporting; the user-facing surface is exercised through Hash/Verify
// in argon2id_test.go.

func TestEncode_FormatsAsPHCString(t *testing.T) {
	t.Parallel()

	p := Params{Memory: 65536, Iterations: 3, Parallelism: 4, SaltLength: 16, KeyLength: 32}
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(255 - i)
	}

	got := encode(p, salt, key)

	require.True(t, strings.HasPrefix(got, "$argon2id$v=19$"), "must start with PHC prefix; got %q", got)
	require.Contains(t, got, "m=65536,t=3,p=4")
	// 5 segments: leading empty (split on $), argon2id, v=19, params, salt, key.
	parts := strings.Split(got, "$")
	require.Len(t, parts, 6)
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	t.Parallel()

	want := Params{Memory: 65536, Iterations: 3, Parallelism: 4, SaltLength: 16, KeyLength: 32}
	salt := []byte("0123456789abcdef")                // 16 bytes
	key := []byte("0123456789abcdef0123456789abcdef") // 32 bytes

	enc := encode(want, salt, key)

	gotP, gotSalt, gotKey, err := decode(enc)
	require.NoError(t, err)
	assert.Equal(t, want.Memory, gotP.Memory)
	assert.Equal(t, want.Iterations, gotP.Iterations)
	assert.Equal(t, want.Parallelism, gotP.Parallelism)
	assert.Equal(t, want.SaltLength, gotP.SaltLength)
	assert.Equal(t, want.KeyLength, gotP.KeyLength)
	assert.Equal(t, salt, gotSalt)
	assert.Equal(t, key, gotKey)
}

func TestDecode_Errors(t *testing.T) {
	t.Parallel()

	// A known-good baseline so we can produce malformed variants from it.
	p := Params{Memory: 65536, Iterations: 3, Parallelism: 4, SaltLength: 16, KeyLength: 32}
	salt := []byte("0123456789abcdef")
	key := []byte("0123456789abcdef0123456789abcdef")
	good := encode(p, salt, key)

	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{
			name:    "empty input",
			input:   "",
			wantErr: ErrInvalidHash,
		},
		{
			name:    "wrong scheme prefix",
			input:   "$argon2i$v=19$m=65536,t=3,p=4$abc$def",
			wantErr: ErrInvalidHash,
		},
		{
			name:    "bcrypt-ish prefix",
			input:   "$2a$12$Vwr3R5OsFtEBQy5Vx1HvhO7dZ4C5QH8I8t6WrFQTzFJxQa7tF6Y2.",
			wantErr: ErrInvalidHash,
		},
		{
			name:    "missing dollar separators (only one)",
			input:   "$argon2id$v=19$m=65536,t=3,p=4_NOT_A_DOLLAR_abc",
			wantErr: ErrInvalidHash,
		},
		{
			name:    "incompatible version v=16",
			input:   "$argon2id$v=16$m=65536,t=3,p=4$" + strings.Split(good, "$")[4] + "$" + strings.Split(good, "$")[5],
			wantErr: ErrIncompatibleVersion,
		},
		{
			name:    "params section unparseable",
			input:   "$argon2id$v=19$not-a-param-block$" + strings.Split(good, "$")[4] + "$" + strings.Split(good, "$")[5],
			wantErr: ErrInvalidHash,
		},
		{
			name:    "missing one parameter",
			input:   "$argon2id$v=19$m=65536,t=3$" + strings.Split(good, "$")[4] + "$" + strings.Split(good, "$")[5],
			wantErr: ErrInvalidHash,
		},
		{
			name:    "non-base64 salt",
			input:   "$argon2id$v=19$m=65536,t=3,p=4$!!!!notb64!!!!$" + strings.Split(good, "$")[5],
			wantErr: ErrInvalidHash,
		},
		{
			name:    "non-base64 key",
			input:   "$argon2id$v=19$m=65536,t=3,p=4$" + strings.Split(good, "$")[4] + "$!!!!notb64!!!!",
			wantErr: ErrInvalidHash,
		},
		{
			name:    "extra trailing segment",
			input:   good + "$extra",
			wantErr: ErrInvalidHash,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, _, err := decode(tt.input)
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

package passwords

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// Hash derives an Argon2id key for password using p and returns the
// PHC-encoded string. A fresh 16-byte (or p.SaltLength) salt is generated
// from crypto/rand on every call, so two Hash calls on the same password
// always produce different output.
//
// Errors:
//   - ErrInvalidParams (wrapped) when p fails Validate.
//   - any error returned by crypto/rand.Read (extremely rare; OS RNG
//     failure). Wrapped with the package prefix so callers can log it.
//
// Hash never panics on its inputs — empty password is acceptable; whether
// to allow it is a policy decision for the caller, not the hashing layer.
func Hash(password string, p Params) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}

	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("passwords: read salt: %w", err)
	}

	key := argon2.IDKey(
		[]byte(password),
		salt,
		p.Iterations,
		p.Memory,
		p.Parallelism,
		p.KeyLength,
	)

	return encode(p, salt, key), nil
}

// Verify reports whether password matches the Argon2id key embedded in the
// PHC-encoded string. The comparison is constant-time via
// crypto/subtle.ConstantTimeCompare so an attacker cannot probe individual
// bytes via timing.
//
// Returns:
//   - (true,  nil)            — password matches the encoded hash.
//   - (false, nil)            — password is well-formed but does not match.
//   - (false, ErrInvalidHash) — encoded is not a valid PHC string;
//     errors.Is(err, ErrInvalidHash) is the canonical check. May also wrap
//     ErrIncompatibleVersion when the version segment is anything other
//     than v=19.
//
// Verify never returns the encoded hash, the candidate password, or any
// recovered key in its error message — those are secrets and must not
// land in logs.
func Verify(encoded, password string) (bool, error) {
	p, salt, key, err := decode(encoded)
	if err != nil {
		return false, err
	}

	candidate := argon2.IDKey(
		[]byte(password),
		salt,
		p.Iterations,
		p.Memory,
		p.Parallelism,
		p.KeyLength,
	)

	return subtle.ConstantTimeCompare(key, candidate) == 1, nil
}

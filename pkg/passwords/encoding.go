package passwords

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidHash is returned when a candidate PHC string is malformed:
// wrong scheme, missing fields, non-base64 salt or key, or any other
// shape mismatch. Verify on a malformed input is (false, ErrInvalidHash);
// callers MUST treat this as "definitely-not-a-match" rather than an
// internal error.
var ErrInvalidHash = errors.New("passwords: invalid hash format")

// ErrIncompatibleVersion is returned when the encoded hash carries a
// version segment other than v=19 (the only version this package emits
// or accepts). It is wrapped through ErrInvalidHash for callers that
// only care "is this a valid hash" — both errors.Is(err, ErrInvalidHash)
// and errors.Is(err, ErrIncompatibleVersion) report true.
var ErrIncompatibleVersion = errors.New("passwords: incompatible argon2 version")

// PHC layout we emit and accept. Locked once and hereafter immutable —
// future param changes go through Params, never through this string.
const (
	phcAlgo    = "argon2id"
	phcVersion = "v=19" // 0x13 == 19
	phcPrefix  = "$" + phcAlgo + "$" + phcVersion + "$"
)

// encode renders the (params, salt, key) triple as a PHC string.
//
// Format:
//
//	$argon2id$v=19$m=<mem>,t=<iter>,p=<par>$<salt-b64>$<key-b64>
//
// where the base64 is RawStdEncoding (no padding). encode is a pure
// function — no I/O — so it never errors.
func encode(p Params, salt, key []byte) string {
	return fmt.Sprintf(
		"%sm=%d,t=%d,p=%d$%s$%s",
		phcPrefix,
		p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// decode parses a PHC string back to (params, salt, key).
//
// Returns ErrInvalidHash on any malformed input, ErrIncompatibleVersion
// (which wraps ErrInvalidHash) when the version segment is anything
// other than v=19. The salt/key length on the returned Params reflect
// the actual decoded byte counts so the caller can re-derive the key
// at the same length without explicitly tracking it.
//
//nolint:gocognit,gocyclo // Linear parsing of a fixed 5-segment format; flattening branches would not help readability.
func decode(s string) (Params, []byte, []byte, error) {
	// Fast path: cheap prefix check before splitting.
	const minimalPrefix = "$argon2id$"
	if !strings.HasPrefix(s, minimalPrefix) {
		return Params{}, nil, nil, ErrInvalidHash
	}

	// Split on '$'. A well-formed PHC string yields exactly 6 segments:
	//   ""          (before the leading '$')
	//   "argon2id"  (algo)
	//   "v=19"      (version)
	//   "m=..,t=..,p=.." (params)
	//   "<salt-b64>" (salt)
	//   "<key-b64>"  (key)
	parts := strings.Split(s, "$")
	const wantParts = 6
	if len(parts) != wantParts {
		return Params{}, nil, nil, ErrInvalidHash
	}
	if parts[0] != "" || parts[1] != phcAlgo {
		return Params{}, nil, nil, ErrInvalidHash
	}

	// Version: only v=19 (0x13) is accepted.
	if parts[2] != phcVersion {
		// Incompatible version; we wrap so callers can detect either
		// the specific reason or the umbrella ErrInvalidHash.
		return Params{}, nil, nil, fmt.Errorf("%w: %w: got %q", ErrInvalidHash, ErrIncompatibleVersion, parts[2])
	}

	// Params: must be exactly "m=<u32>,t=<u32>,p=<u8>" — the trailing
	// %s gates Sscanf from accepting prefixes like "m=1,t=2".
	var (
		mem  uint32
		iter uint32
		par  uint8
		tail string
	)
	n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d%s", &mem, &iter, &par, &tail)
	// We expect Sscanf to fail with "unexpected EOF" once the three
	// numbers are consumed and there is no %s match. If three values
	// parsed cleanly that's success; anything else is malformed.
	if !(n == 3 && err != nil && tail == "") {
		return Params{}, nil, nil, fmt.Errorf("%w: params %q: %w", ErrInvalidHash, parts[3], err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: salt: %w", ErrInvalidHash, err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: key: %w", ErrInvalidHash, err)
	}

	p := Params{
		Memory:      mem,
		Iterations:  iter,
		Parallelism: par,
		//nolint:gosec // base64-decoded length cannot exceed input size; PHC strings are
		// hundreds of bytes max, far below uint32 max. G115 false positive on int->uint32.
		SaltLength: uint32(len(salt)),
		//nolint:gosec // same reasoning as SaltLength above.
		KeyLength: uint32(len(key)),
	}
	return p, salt, key, nil
}

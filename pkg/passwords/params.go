package passwords

import (
	"errors"
	"fmt"
)

// ErrInvalidParams is the umbrella sentinel for any Params.Validate failure.
// Specific reasons are wrapped through %w; callers may errors.Is against this
// value to detect "the parameters are bogus" without inspecting the message.
var ErrInvalidParams = errors.New("passwords: invalid params")

// Params controls Argon2id cost and output size.
//
// Zero values are invalid; always start from DefaultParams() and tune from
// there. The Argon2 spec (RFC 9106 §3.1) defines these:
//
//   - Memory      — m (kibibytes). Linear factor of memory used.
//   - Iterations  — t (passes). Linear factor of CPU time.
//   - Parallelism — p (lanes). Splits work across cores; m must be >= 8*p.
//   - SaltLength  — s (bytes). Spec recommends >= 16 for password hashing.
//   - KeyLength   — T (bytes). Output length; >= 4 per spec.
//
// All fields are part of the encoded PHC string except SaltLength/KeyLength
// which are recovered at decode time from the actual byte lengths.
type Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32 // bytes
	KeyLength   uint32 // bytes
}

// DefaultParams returns the production defaults.
//
//	Memory      = 19 * 1024 KiB  (19 MiB)
//	Iterations  = 2
//	Parallelism = 1
//	SaltLength  = 16 bytes
//	KeyLength   = 32 bytes
//
// These are the OWASP "Argon2id minimum" recommendation (Password Storage
// Cheat Sheet, Aug 2024 revision) — sized for a deployment where each login
// request triggers one hash and the host may receive bursty traffic. The
// 19 MiB working set fits comfortably in a 256 MiB pod and is small enough
// that BoundedHasher (see bounded.go) can cap concurrency without wedging.
//
// We deliberately diverged from the "max security" 64 MiB / t=3 / p=4 profile
// to lower the OOM/DoS surface: an unbounded burst of N concurrent hashes at
// 64 MiB each pegs memory linearly with no operational benefit for our threat
// model (no high-value password targets, no expectation of an offline attack
// against a future DB dump where seconds-per-guess matters more than
// MB-per-guess). With m=19 MiB and BoundedHasher capping concurrency at the
// CPU count, the worst-case resident set is bounded and predictable.
//
// Spec note: these values can be overridden via auth.password.{memory,t,p}
// in the runtime config — production may tune up if hardware permits.
func DefaultParams() Params {
	return Params{
		Memory:      19 * 1024,
		Iterations:  2,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// BackupCodeParams returns Argon2id parameters tuned for hashing
// short single-use tokens (e.g. TOTP backup codes), NOT user passwords.
//
//	Memory      = 1 * 1024 KiB   (1 MiB)
//	Iterations  = 1
//	Parallelism = 1
//	SaltLength  = 16 bytes
//	KeyLength   = 32 bytes
//
// Backup codes in our system are 10 hex characters = 40 bits of entropy.
// Even cheap Argon2id raises the per-trial cost enough that an offline
// brute-force of all 2^40 candidates against an exfiltrated hash takes
// weeks on commodity GPUs — we don't need full password-strength
// memory-hardness here, and ten of these are hashed up front during
// TOTP enroll. Using DefaultParams (19 MiB) would burn ~190 MiB and
// 200 ms per Enroll request; this profile keeps Enroll well under
// 50 ms while still meeting the offline-attack bar.
//
// If a future feature uses these params for anything that resembles a
// real password (something the user types repeatedly, or with a
// guessable structure), revisit — those need DefaultParams or stronger.
func BackupCodeParams() Params {
	return Params{
		Memory:      1 * 1024,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// Validate returns nil if all fields are within sane bounds, otherwise an
// error wrapping ErrInvalidParams with a low-cardinality reason string.
//
// Bounds enforced:
//
//   - Memory, Iterations, Parallelism, SaltLength, KeyLength must all be > 0.
//   - Memory must be >= 8 * Parallelism (Argon2 spec floor — RFC 9106 §3.1).
//   - KeyLength must be >= 4 (Argon2 spec floor).
//
// We deliberately do not impose upper bounds: cost-vs-throughput is the
// caller's concern, and the cost is bounded in practice by request timeouts.
func (p Params) Validate() error {
	switch {
	case p.Memory == 0:
		return fmt.Errorf("%w: memory must be > 0", ErrInvalidParams)
	case p.Iterations == 0:
		return fmt.Errorf("%w: iterations must be > 0", ErrInvalidParams)
	case p.Parallelism == 0:
		return fmt.Errorf("%w: parallelism must be > 0", ErrInvalidParams)
	case p.SaltLength == 0:
		return fmt.Errorf("%w: salt length must be > 0", ErrInvalidParams)
	case p.KeyLength == 0:
		return fmt.Errorf("%w: key length must be > 0", ErrInvalidParams)
	case p.KeyLength < 4:
		return fmt.Errorf("%w: key length must be >= 4", ErrInvalidParams)
	case p.Memory < 8*uint32(p.Parallelism):
		return fmt.Errorf("%w: memory must be >= 8 * parallelism", ErrInvalidParams)
	}
	return nil
}

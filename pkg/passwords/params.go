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

// DefaultParams returns the production defaults from spec §14.2:
//
//	Memory      = 64 * 1024 KiB  (64 MiB)
//	Iterations  = 3
//	Parallelism = 4
//	SaltLength  = 16 bytes
//	KeyLength   = 32 bytes
func DefaultParams() Params {
	return Params{
		Memory:      64 * 1024,
		Iterations:  3,
		Parallelism: 4,
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

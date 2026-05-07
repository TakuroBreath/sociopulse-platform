package encryption

import (
	"crypto/hmac"
	"crypto/sha256"
	"strings"
)

// PepperLen is the minimum acceptable per-tenant pepper length in bytes.
// HMAC-SHA256's internal block is 64 bytes; the spec mandates 32 random
// bytes per tenant which is more than enough entropy.
const PepperLen = 32

// PhoneHasher computes deterministic HMAC-SHA256 hashes of phone numbers
// using a per-tenant pepper. It is safe for concurrent use; the pepper
// is copied defensively at construction and never mutated thereafter.
type PhoneHasher struct {
	pepper []byte
}

// NewPhoneHasher constructs a PhoneHasher. The pepper MUST be at least
// 32 bytes (the spec specifies "random 32 bytes per-tenant"). Shorter
// peppers panic — there's no recoverable scenario where a too-short
// pepper is intentional.
//
// The pepper is copied into the hasher; the caller may mutate or zero
// their own buffer afterwards without affecting future Hash calls.
func NewPhoneHasher(pepper []byte) *PhoneHasher {
	if len(pepper) < PepperLen {
		panic("encryption: phone hash pepper must be >= 32 bytes")
	}
	cp := make([]byte, len(pepper))
	copy(cp, pepper)
	return &PhoneHasher{pepper: cp}
}

// Hash returns the 32-byte HMAC-SHA256 of the normalised phone number.
//
// The output is deterministic for a fixed (pepper, phone) pair, so it is
// suitable for unique-index lookups. It is also irreversible without the
// pepper, so direct exposure of the column doesn't leak the underlying
// phone number to anyone without the per-tenant pepper.
func (h *PhoneHasher) Hash(phone string) []byte {
	mac := hmac.New(sha256.New, h.pepper)
	// Writes to crypto/hmac never error.
	mac.Write([]byte(NormalizePhone(phone)))
	return mac.Sum(nil)
}

// NormalizePhone canonicalises Russian phone numbers to E.164 form
// "+7XXXXXXXXXX". It strips spaces, dashes, parentheses, and similar
// formatting characters; converts a leading "8" or "7" prefix to "+7";
// and prepends "+7" to a bare 10-digit local number. Inputs that don't
// match any Russian-pattern fall through unchanged so the caller can
// detect downstream — the hasher is the wrong place to enforce input
// validation; that's the import pipeline's job.
func NormalizePhone(phone string) string {
	digits := strings.Builder{}
	digits.Grow(len(phone))
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	switch {
	case len(d) == 11 && d[0] == '8':
		return "+7" + d[1:]
	case len(d) == 11 && d[0] == '7':
		return "+7" + d[1:]
	case len(d) == 10:
		return "+7" + d
	default:
		return phone
	}
}

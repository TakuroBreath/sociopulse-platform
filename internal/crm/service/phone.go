package service

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/nyaruka/phonenumbers"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
)

// defaultPhoneRegion is the libphonenumber default region we feed to the
// parser when an input lacks a country code. Russia is the only target
// market for v1; the constant is here so future multi-country support
// requires changing one place.
const defaultPhoneRegion = "RU"

// NormalizedPhone is the post-validation phone representation. Callers
// receive this struct (rather than a bare string) so the type system
// forbids passing a raw, possibly-malformed phone string into
// hash/encrypt/store call sites — those accept .E164 explicitly.
type NormalizedPhone struct {
	// E164 is the canonical international form, e.g. "+79161234567".
	// Always starts with a '+', followed by a country code and the
	// subscriber number. Used for hashing and encryption-at-rest.
	E164 string

	// Region is the ISO-3166-1 alpha-2 region code the parser used; we
	// always pass "RU" today, so this field is "RU" for every successful
	// parse. Captured here so the store row records what region the
	// service treated the number as (matters when v2 adds non-RU
	// regions).
	Region string
}

// NormalizeRussianPhone parses input as a Russian phone number and
// returns its canonical E.164 form. Accepted inputs:
//
//   - "+79161234567"            (already canonical)
//   - "79161234567"             (no plus)
//   - "89161234567"             (Russian domestic prefix `8`)
//   - "8 (916) 123-45-67"       (operator paste from spreadsheet)
//   - "+7 916 123 45 67"        (with regular or non-breaking spaces)
//   - "+7—916—123—45—67"        (em-dashes — Excel auto-formats them)
//   - any of the above wrapped in parentheses or surrounded by whitespace.
//
// Returns api.ErrInvalidPhone (wrapped via fmt.Errorf with %w) for any
// input the libphonenumber parser rejects, anything that doesn't validate
// as a Russian number (region "RU"), or any output that fails the
// canonical IsValidNumber check. The error message is intentionally
// low-cardinality and PII-free — the offending phone is NEVER included
// in the message.
//
// We intentionally do NOT roll our own E.164 normaliser. libphonenumber
// catches edge cases (8-as-leading, double-leading-zero, RFC3966 form,
// alpha-numeric vanity numbers) that hand-written sanitisers
// consistently miss; the cost of a Go port is one library import.
func NormalizeRussianPhone(input string) (NormalizedPhone, error) {
	cleaned := sanitisePhoneInput(input)
	if cleaned == "" {
		return NormalizedPhone{}, fmt.Errorf("normalize phone: empty input: %w", crmapi.ErrInvalidPhone)
	}

	num, err := phonenumbers.Parse(cleaned, defaultPhoneRegion)
	if err != nil {
		// libphonenumber returns its own sentinel set
		// (ErrTooShortNSN, ErrNotANumber, etc.). We collapse all of
		// them under api.ErrInvalidPhone — callers care about the
		// "invalid phone" branch, not which specific parse failure
		// triggered it. The original error is dropped on purpose to
		// avoid leaking parser internals into structured logs.
		_ = err
		return NormalizedPhone{}, fmt.Errorf("normalize phone: parse failed: %w", crmapi.ErrInvalidPhone)
	}

	// IsValidNumberForRegion catches both the "valid E.164 from
	// another country" case (e.g. a US number "+14155551234" parses
	// cleanly but isn't a Russian number) AND the
	// length/structure-invalid-for-RU case. We rely on it as the
	// authoritative validity check; the broader IsValidNumber would
	// be redundant since every successful per-region validation also
	// satisfies the global check.
	if !phonenumbers.IsValidNumberForRegion(num, defaultPhoneRegion) {
		return NormalizedPhone{}, fmt.Errorf("normalize phone: not valid for RU: %w", crmapi.ErrInvalidPhone)
	}

	return NormalizedPhone{
		E164:   phonenumbers.Format(num, phonenumbers.E164),
		Region: defaultPhoneRegion,
	}, nil
}

// sanitisePhoneInput strips every code point that isn't a digit or '+'
// before passing the result to libphonenumber. The parser tolerates
// most whitespace and punctuation, but Excel-produced inputs frequently
// embed exotic characters (NBSP U+00A0, em-dash U+2014, en-dash U+2013,
// figure-dash U+2012) that defeat the parser's heuristics. Stripping
// upfront makes the parser's job deterministic.
//
// We retain '+' since libphonenumber uses it as the unambiguous
// "country-code follows" signal — without it, the parser falls back to
// the supplied default region.
func sanitisePhoneInput(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	for _, r := range in {
		switch {
		case unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '+':
			b.WriteRune(r)
		default:
			// drop everything else (whitespace, parens, dashes, NBSP,
			// em-dash, en-dash, figure-dash, dots, etc.)
		}
	}
	return b.String()
}

// errPhoneInvalid is reserved for future internal wrapping if we need
// to discriminate between "input parse failed" and "input not a Russian
// number" in tests. Today we collapse both under api.ErrInvalidPhone via
// errors.Is. The blank import keeps `errors` reachable so future code
// in this file can use it without a churn-y diff.
var _ = errors.New

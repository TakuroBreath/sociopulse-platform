package encryption

// NormalizePhone normalises a raw phone-number string to E.164.
// It strips formatting characters, validates the country code, and
// rejects obviously bogus inputs (too short, non-digit body).
//
// The full normalisation rules (Russian +7 vs 8, leading whitespace,
// international diallng prefixes) are documented in Plan 03 Task 5.
func NormalizePhone(raw string) (string, error) {
	panic("not implemented: see Plan 03 Task 5")
}

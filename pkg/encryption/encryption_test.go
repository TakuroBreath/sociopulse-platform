package encryption

import "testing"

// TestEncryptionCompiles is a placeholder smoke test that validates the
// package compiles. Real algorithm tests land in Plan 03 Task 5.
func TestEncryptionCompiles(t *testing.T) {
	t.Parallel()

	// Compile-time interface check.
	var _ PhoneHasher = (PhoneHasher)(nil)
}

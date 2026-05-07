package config

import "testing"

// TestConfigCompiles is a placeholder smoke test that validates the
// package compiles. Real validation tests load a YAML fixture and
// assert sub-struct decoding in Plan 02 Task 1.
func TestConfigCompiles(t *testing.T) {
	t.Parallel()

	// Construct a zero Config to make sure the struct shape is valid.
	_ = Config{}
}

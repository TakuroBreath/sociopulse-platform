package auth

import "testing"

// TestAuthMiddlewareCompiles is a placeholder smoke test that
// validates the package compiles. Real middleware tests
// (httptest + a fake ClaimsValidator) land in Plan 04 Task 4.
func TestAuthMiddlewareCompiles(t *testing.T) {
	t.Parallel()

	// Compile-time check that the constant has the documented type.
	var _ string = ClaimsContextKey
}

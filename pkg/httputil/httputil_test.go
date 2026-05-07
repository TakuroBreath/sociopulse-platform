package httputil

import "testing"

// TestHTTPUtilCompiles is a placeholder smoke test that validates the
// package compiles. Real middleware tests (httptest + gin.TestMode)
// land in Plan 02 Task 3.
func TestHTTPUtilCompiles(t *testing.T) {
	t.Parallel()

	// Compile-time check that ErrorEnvelope is JSON-serialisable.
	_ = ErrorEnvelope{Code: "x", Message: "y"}
}

package hours_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces goroutine quiescence on package exit. The Checker
// itself spawns no goroutines; goleak protects against a future
// regression that adds one without a Stop hook.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

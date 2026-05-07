package store_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the auth store test
// binary. The store does not own goroutines today; the guard catches
// regressions if a future refactor introduces one.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

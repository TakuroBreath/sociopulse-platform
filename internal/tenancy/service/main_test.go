package service_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the tenancy service test
// binary. The service does not own goroutines today; the guard catches
// regressions if a future refactor introduces a background workers without
// a cancel path.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

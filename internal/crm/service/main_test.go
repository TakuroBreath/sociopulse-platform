package service_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the crm service test
// binary. ProjectService doesn't spawn goroutines today; the guard is
// here to catch a regression if any future refactor adds a worker
// without a cancel path (e.g. an async audit publisher).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

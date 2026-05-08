package dialer_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces goroutine quiescence on package exit. The PubSub
// itself spawns no goroutines; goleak protects against future
// regressions (e.g. a Module helper that leaks a watchdog under test
// without a matching Stop).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

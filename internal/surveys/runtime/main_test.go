package runtime_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine-leak guard for the runtime test
// binary. The runtime is purely synchronous (no goroutines, no I/O),
// so this guard exists to catch a future regression that adds a
// background pre-compiler / async cache warmer without a clean cancel
// path. Same pattern as dsl, schemavalidator, service test binaries.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

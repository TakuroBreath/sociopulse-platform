package http_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the surveys transport
// test binary. Handlers themselves don't spawn goroutines; the guard
// catches a regression if any future handler accidentally launches a
// worker without a cancel path.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

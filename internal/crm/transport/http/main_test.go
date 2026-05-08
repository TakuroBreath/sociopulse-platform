package http_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the crm transport
// test binary. Handlers don't spawn goroutines themselves, but gin's
// internal request lifecycle and miniredis-backed fakes used in some
// tests do — the guard catches a regression if any handler
// accidentally launches a worker without a cancel path.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

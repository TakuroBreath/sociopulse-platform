package nats_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the billing NATS
// transport test binary. The subscriber registers a single handler on
// the bus; tests exercise the handler synchronously via a fake bus so
// no goroutines are spawned. A regression that introduces a stray
// goroutine surfaces here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

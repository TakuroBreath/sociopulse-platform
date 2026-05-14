package nats_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the dialer NATS
// transport test binary. The subscriber registers JetStream handlers
// that run on the bus's dispatcher goroutine; a regression that
// forgets to drain on shutdown would leak that goroutine and
// goleak.VerifyTestMain would catch it here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

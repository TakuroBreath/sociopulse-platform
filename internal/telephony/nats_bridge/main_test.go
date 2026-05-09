package nats_bridge_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs goleak as defence-in-depth. The bridge spawns a single
// event-publisher goroutine; the cmd-subscriber's per-message goroutines
// are owned by the underlying eventbus.Subscriber. Any leaked goroutine
// inside the package's own tests is a regression and fails this guard.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

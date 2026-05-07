package eventbus

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain ensures pkg/eventbus does not leak goroutines.
// Implementations spawn goroutines for JetStream consumers in Plan 03
// Task 7, so we install the leak guard up front.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestEventBusCompiles is a placeholder smoke test that validates the
// package compiles and its interfaces are well-formed.
func TestEventBusCompiles(t *testing.T) {
	t.Parallel()

	var (
		_ Publisher  = (*NATSPublisher)(nil)
		_ Subscriber = (*NATSSubscriber)(nil)
	)
}

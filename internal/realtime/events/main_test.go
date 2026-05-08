package events_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs goleak as defence-in-depth. The realtime/events
// package itself spawns no goroutines — every dispatcher goroutine is
// owned by the underlying eventbus.Subscriber — but a future regression
// that adds a stray goroutine here would silently leak without this
// guard.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

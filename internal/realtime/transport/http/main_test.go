package http

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine-leak guard for the realtime
// transport/http test binary. The /ws handler spawns 4 goroutines per
// connection (reader / writer / pinger / Touch ticker); a forgotten
// Close on any of them would leak across test boundaries and trip
// goleak here. Force-action and listen-in handlers are synchronous so
// they don't need their own guard, but the WS tests ride on this one.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

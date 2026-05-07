package config

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain ensures pkg/config does not leak goroutines. The hot-reload
// fsnotify watcher spawns a goroutine; tests using HotReload must invoke
// Snapshot.Close (typically via t.Cleanup) so the goroutine exits before
// goleak inspects the runtime.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

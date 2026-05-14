package http_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the reports transport
// test binary. The handlers are synchronous (no spawned goroutines), so
// any leak surfaces a regression.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

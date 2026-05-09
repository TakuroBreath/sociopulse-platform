package http_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the recording transport
// test binary. The handlers are synchronous (the streaming endpoint uses
// io.Copy on the gin response writer with no spawned goroutines), so a
// goroutine leak here would indicate a regression.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

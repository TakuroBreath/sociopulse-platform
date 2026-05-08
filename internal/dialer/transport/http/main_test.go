package http_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the dialer transport
// test binary. The HTTP handlers are synchronous, but the WebSocket
// handler spawns one reader goroutine per connection — TestMain
// ensures every test cleans up its WS connections so a regression
// (e.g. forgotten cancel on Subscribe) is caught here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

package service_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the auth service test
// binary. UserService and JWTIssuer don't spawn goroutines today;
// Authenticator dispatches no background workers. The guard is here to
// catch a regression if any future refactor adds a worker without a
// cancel path (e.g. an async audit publisher).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

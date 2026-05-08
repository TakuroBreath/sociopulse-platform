package service_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the surveys service
// test binary. SurveyService doesn't spawn goroutines today; the
// guard catches a regression if any future refactor adds a worker
// without a cancel path.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

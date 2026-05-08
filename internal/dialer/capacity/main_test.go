//go:build !integration

package capacity_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces goroutine quiescence on package exit for the
// default (unit) test build. The integration build sources its own
// TestMain from tracker_integration_test.go.
//
// The Tracker itself spawns no goroutines; goleak protects against a
// future regression that adds one without a Stop hook.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

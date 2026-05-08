//go:build !integration

package retry_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces goroutine quiescence on package exit for the
// default (unit) test build. The integration build sources its own
// TestMain from leader_election_integration_test.go so the unit /
// integration test binaries don't collide on a duplicate TestMain.
//
// Run blocks; the unit tests cancel its ctx and wait for the goroutine
// to exit before TestMain runs goleak. A regression that leaks (e.g. a
// stray ticker without defer Stop) surfaces here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

//go:build integration

package store_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the surveys store
// integration-test binary. The store does not own goroutines today;
// the guard catches regressions if a future refactor introduces a
// background worker without a cancel path. Gated by the integration
// build tag so unit-test runs (which don't compile this file) aren't
// affected.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

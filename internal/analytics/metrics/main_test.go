package metrics_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces goroutine quiescence on package exit. The metrics
// package spawns no goroutines of its own; goleak protects against
// future regressions (e.g. a metrics-driven async pusher).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

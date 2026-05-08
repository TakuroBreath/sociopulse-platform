package dsl_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine-leak guard for the dsl test binary.
// The Task 2 stub never spawns a goroutine; the guard is here so the
// regression bites the first PR that adds an async parser worker
// without a cancel path.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

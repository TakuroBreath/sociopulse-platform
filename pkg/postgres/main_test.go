package postgres_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine leak guard for the whole pkg/postgres
// test binary (both unit and integration suites). pgxpool spawns
// background goroutines per pool instance — every test that calls
// postgres.Open MUST also call pool.Close (typically via t.Cleanup) or
// goleak will fail the run.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

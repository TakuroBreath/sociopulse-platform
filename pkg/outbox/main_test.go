package outbox

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain ensures pkg/outbox does not leak goroutines. The relay
// runs an indefinite loop in production (Plan 03 Task 6) — if a test
// constructs a Relay it must cancel the context and wait for Run to
// return.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestOutboxCompiles is a placeholder smoke test that validates the
// package compiles and the contract surfaces are well-formed.
func TestOutboxCompiles(t *testing.T) {
	t.Parallel()

	var _ Writer = (*PostgresWriter)(nil)
}

package schemavalidator_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the goroutine-leak guard for the schemavalidator
// test binary. The validator is purely declarative — no goroutines —
// so this guard exists to catch a future regression where someone
// adds an async cache warmer or background DSL pre-compiler without
// a clean cancel path (per project policy
// `07-go-coding-standards § Concurrency`).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

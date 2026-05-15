//go:build smoke

package smoke_test

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/sociopulse/platform/tests/smoke"
)

// TestMain governs the smoke harness self-test binary. It:
//
//   - Runs every Test* in the package.
//   - Drains testcontainers + closes the cached *postgres.Pool via
//     TerminateOnTestMainCleanup BEFORE checking for goroutine leaks
//     (the pool's backgroundHealthCheck goroutine stays alive until
//     Close, so a naive goleak.VerifyTestMain would always fail).
//   - Asserts goroutine cleanliness via goleak.Find with OTel
//     suppressions mirroring cmd/api/main_test.go.
//
// We do NOT use goleak.VerifyTestMain — that helper calls os.Exit
// internally, so any defer for teardown would never fire and the
// cached pool / containers would orphan on every run.
//
// Plan 21b Task 1 fix-up (review): the original Task 1 commit (190a0d9)
// added a *postgres.Pool to Stack but no TestMain, so the pool + the
// testcontainers orphaned on process exit. This file closes that gap.
// The order matters: drain THEN goleak. Without TerminateOnTestMainCleanup
// running first, the pool's pgxpool.backgroundHealthCheck goroutine
// false-positives the leak check.
func TestMain(m *testing.M) {
	code := m.Run()

	// Drain the testcontainer + pool teardowns BEFORE the leak check.
	// TerminateOnTestMainCleanup runs every fn that addProcessTeardown
	// registered (pgPool.Close, then each container's Terminate).
	smoke.TerminateOnTestMainCleanup()

	// Mirror cmd/api/main_test.go's OTel ignore list verbatim. The
	// batchSpanProcessor + OTLP grpc client retry goroutines stay up
	// across the goleak window because the smoke config points at
	// localhost:4317 with no real collector; in production with a
	// reachable endpoint they shut down promptly.
	if err := goleak.Find(
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc/internal/retry.wait"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc.(*client).exportContext.func1"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).Shutdown.func1.1"),
	); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	os.Exit(code)
}

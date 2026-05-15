//go:build smoke

package main

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/sociopulse/platform/tests/smoke"
)

// TestMain governs the cmd/api smoke test binary. It:
//
//   - Runs every Test* in the cmd/api package (smoke-tagged + the
//     unconditional unit tests that compile under any tag).
//   - Drains testcontainers + closes the cached *postgres.Pool via
//     smoke.TerminateOnTestMainCleanup BEFORE checking for goroutine
//     leaks. Stack.PgPool spawns pgxpool's backgroundHealthCheck which
//     stays alive until pool.Close — a naive goleak.VerifyTestMain
//     would always fail once any smoke scenario calls Stack.PgPool.
//   - Asserts goroutine cleanliness via goleak.Find with the same OTel
//     suppressions as the !smoke TestMain (testmain_default_test.go) so
//     the leak-detector contract stays uniform across both binaries.
//
// We do NOT use goleak.VerifyTestMain — that helper calls os.Exit
// internally, so any defer for teardown would never fire and the cached
// pool + containers would orphan on every run. Mirrors the established
// pattern in tests/smoke/main_test.go::TestMain.
//
// Plan 21b Task 6 wired this. Prior to Task 6 the cmd/api smoke test
// binary inherited the !smoke TestMain's goleak.VerifyTestMain; any
// scenario that consumed Stack.PgPool tripped the leak guard. Task 4
// worked around it by using pgx.Connect inline (see
// smoke_operator_ws_test.go's FSM cleanup); Task 6 fixes the root cause
// here so future smoke scenarios consume Stack.PgPool without ceremony.
func TestMain(m *testing.M) {
	code := m.Run()

	// Drain the testcontainer + pool teardowns BEFORE the leak check.
	// TerminateOnTestMainCleanup runs every fn that addProcessTeardown
	// registered (pgPool.Close, then each container's Terminate).
	smoke.TerminateOnTestMainCleanup()

	// Mirror testmain_default_test.go's OTel ignore list verbatim. The
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

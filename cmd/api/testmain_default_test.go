//go:build !smoke

package main

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain ensures cmd/api boot/shutdown does not leak goroutines.
//
// Tests boot a real *trace.TracerProvider whose OTLP exporter retries
// indefinitely against a missing collector at localhost:4317. On shutdown
// the batchSpanProcessor's drain blocks in the retry's wait() until the
// shutdown context expires, leaving short-lived OTel goroutines around when
// goleak inspects the runtime. We ignore those — they exit on their own
// once the retry's context deadline fires; in production with a reachable
// collector they exit promptly.
//
// This is the !smoke variant — used by `go test ./cmd/api/...` without
// the smoke build tag. The smoke variant (testmain_smoke_test.go) runs
// m.Run() manually and drains tests/smoke's testcontainer + pool
// teardown BEFORE the leak check, which goleak.VerifyTestMain (which
// invokes os.Exit internally and skips deferred cleanup) cannot do.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc/internal/retry.wait"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc.(*client).exportContext.func1"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).Shutdown.func1.1"),
	)
}

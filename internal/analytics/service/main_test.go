package service_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enables goleak's goroutine-leak detector across every test
// in this package, including the //go:build integration suite. Goleak
// reports any goroutine that outlives a test as a leak — the most
// common offender here is the IngestPipeline ticker goroutine if Run
// fails to honour ctx.Done.
//
// Optional ignores mirror internal/analytics/store/main_test.go:
//   - testcontainers reaper + docker HTTP/2 background reader for the
//     integration build.
//   - quic-go server goroutine pulled in by nats transitive deps.
//
// The broad `runtime_pollWait` ignore is deliberately avoided — that's
// the same top-of-stack the offender would have if the pipeline leaked
// a NATS subscriber.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(
		m,
		// testcontainers reaper goroutine — owned by the lib, not us.
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		// docker HTTP/2 background reader (specific package path).
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		// nats / quic-go internal goroutines spawned by transitive deps
		// pulled in by testcontainers / embedded NATS server; they are
		// not part of our code path.
		goleak.IgnoreAnyFunction("github.com/quic-go/quic-go.(*baseServer).run"),
	)
}

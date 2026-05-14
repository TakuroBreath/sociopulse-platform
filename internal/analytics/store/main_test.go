package store_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enables goleak's goroutine-leak detector across every test in
// this package, including the //go:build integration suite. Goleak
// reports any goroutine that outlives a test as a leak — the most
// common offender here is a clickhouse-go conn pool that was forgotten
// at Close(). Catching leaks here keeps the ingest pipeline (which
// keeps long-lived *Conn instances per process) from accumulating
// silent goroutine garbage.
//
// Optional ignores: testcontainers-go spawns persistent goroutines for
// reaper / log streaming that we do not own. They are ignored by name
// so we stay focused on OUR code's leaks. Adjust the list if a new
// testcontainers internal goroutine surfaces in a future bump.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(
		m,
		// testcontainers reaper goroutine — owned by the lib, not us.
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		// HTTP/2 transport background reader used by the docker client.
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		// nats / quic-go internal goroutines spawned by transitive deps
		// pulled in by testcontainers; they are not part of our code path.
		goleak.IgnoreAnyFunction("github.com/quic-go/quic-go.(*baseServer).run"),
	)
}

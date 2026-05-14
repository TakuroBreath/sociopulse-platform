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
// Optional ignores: testcontainers-go + transitive deps spawn
// persistent goroutines for reaper / log streaming / docker control
// that we do not own. They are ignored by SPECIFIC top-of-stack
// function so a leaked clickhouse-go pool connection (which would
// also park in netpoll → `internal/poll.runtime_pollWait`) is NOT
// masked. The broad `runtime_pollWait` ignore is deliberately
// avoided — that's the same top-of-stack the offender would have.
//
// Adjust the list if a new testcontainers/docker internal goroutine
// surfaces in a future bump. Run with goleak strict locally to
// observe new patterns before adding to this list.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(
		m,
		// testcontainers reaper goroutine — owned by the lib, not us.
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		// docker HTTP/2 background reader (specific package path, NOT
		// the generic `runtime_pollWait` — that would mask CH pool leaks).
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		// nats / quic-go internal goroutines spawned by transitive deps
		// pulled in by testcontainers; they are not part of our code path.
		goleak.IgnoreAnyFunction("github.com/quic-go/quic-go.(*baseServer).run"),
	)
}

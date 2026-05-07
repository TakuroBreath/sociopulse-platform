package grpc

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain ensures pkg/grpc does not leak goroutines. The constructors
// here will spawn keepalive / accept loops in production wiring (Plan
// 02 Task 4), so we install the leak guard up front.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestGRPCCompiles is a placeholder smoke test that validates the
// package compiles. Real client/server tests with bufconn live next to
// the consumers (recording, telephony) per docs/architecture/04.
func TestGRPCCompiles(t *testing.T) {
	t.Parallel()
}

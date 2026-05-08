package rdd

import (
	"testing"

	"go.uber.org/zap/zaptest"
)

// TestNewChaCha8Seeded_DistinctStreams — two consecutive seed calls
// must produce ChaCha8 streams that differ within the first 8 bytes
// of output. crypto/rand has 256 bits of entropy per call; the
// probability that two 32-byte seeds collide is 2^-256 — effectively
// zero. We sample 8 bytes from each stream and assert non-equality
// to pin the contract that seeding is no longer wall-clock-driven.
func TestNewChaCha8Seeded_DistinctStreams(t *testing.T) {
	t.Parallel()

	logger := zaptest.NewLogger(t)

	src1 := newChaCha8Seeded(logger)
	src2 := newChaCha8Seeded(logger)

	var b1, b2 [8]byte
	src1.Read(b1[:])
	src2.Read(b2[:])

	if b1 == b2 {
		t.Fatalf("two consecutive newChaCha8Seeded() calls produced identical 8-byte output: %x", b1)
	}
}

// TestNewChaCha8Seeded_NilLoggerOK — nil logger is tolerated. This
// gives the constructor a degraded but functional path when wired
// without a logger (Config.Logger nil → zap.NewNop, but the helper
// itself must also stay nil-safe so misuse is never a panic).
func TestNewChaCha8Seeded_NilLoggerOK(t *testing.T) {
	t.Parallel()

	src := newChaCha8Seeded(nil)
	if src == nil {
		t.Fatal("newChaCha8Seeded(nil) returned nil source")
	}
	var b [4]byte
	src.Read(b[:])
}

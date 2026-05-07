package passwords

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/semaphore"
)

// ErrHasherBusy is returned by BoundedHasher when the caller's context
// expires while waiting for a free hashing slot. Callers handling HTTP
// requests should map this to 503 Service Unavailable with a Retry-After
// header — the system is healthy, just temporarily saturated.
var ErrHasherBusy = errors.New("passwords: hasher busy (concurrency cap reached)")

// BoundedHasher caps the number of in-flight Hash/Verify calls to a
// configured ceiling. It exists because every Argon2id derivation
// allocates the full Memory working set (DefaultParams ~= 19 MiB), so an
// unbounded burst of concurrent logins — organic OR malicious — can OOM
// a small pod long before any per-IP rate limiter kicks in.
//
// The ceiling is enforced via a weighted semaphore. Acquire respects the
// caller's context, so handlers under deadline pressure surface
// ErrHasherBusy instead of piling up goroutines blocked on the lock.
//
// Typical wiring:
//
//	inner := passwords.Default()
//	hasher := passwords.NewBoundedHasher(inner, runtime.NumCPU())
//
// Sizing guidance:
//
//   - 1 slot per CPU is a safe default — Argon2 is CPU-bound, additional
//     concurrency only adds context-switching overhead.
//   - The worst-case resident set is roughly maxConcurrent * Memory.
//     With DefaultParams (19 MiB) and NumCPU=4, that's ~76 MiB — well
//     within a 256 MiB pod even with the rest of the binary loaded.
//   - The pgx pool, http server, and other goroutines do NOT count
//     against this limit — they don't hash.
type BoundedHasher struct {
	inner Hasher
	sem   *semaphore.Weighted
}

// Compile-time interface conformance.
var _ Hasher = (*BoundedHasher)(nil)

// NewBoundedHasher wraps inner so that no more than maxConcurrent calls
// to Hash or Verify run at once. maxConcurrent <= 0 is normalized to 1
// to keep buggy initialization (e.g. runtime.NumCPU returning 0 in some
// odd containers) from wedging the auth flow.
func NewBoundedHasher(inner Hasher, maxConcurrent int) *BoundedHasher {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &BoundedHasher{
		inner: inner,
		sem:   semaphore.NewWeighted(int64(maxConcurrent)),
	}
}

// Hash acquires a slot then delegates to the inner Hasher. If the
// context expires while waiting, returns ErrHasherBusy wrapping the
// context error.
func (b *BoundedHasher) Hash(ctx context.Context, password string) (string, error) {
	if err := b.sem.Acquire(ctx, 1); err != nil {
		return "", fmt.Errorf("%w: %w", ErrHasherBusy, err)
	}
	defer b.sem.Release(1)
	return b.inner.Hash(ctx, password)
}

// Verify acquires a slot then delegates to the inner Hasher. If the
// context expires while waiting, returns ErrHasherBusy wrapping the
// context error.
func (b *BoundedHasher) Verify(ctx context.Context, encoded, password string) (bool, error) {
	if err := b.sem.Acquire(ctx, 1); err != nil {
		return false, fmt.Errorf("%w: %w", ErrHasherBusy, err)
	}
	defer b.sem.Release(1)
	return b.inner.Verify(ctx, encoded, password)
}

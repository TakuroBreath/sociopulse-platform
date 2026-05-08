package esl

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff implements jittered exponential delay between reconnect
// attempts. It is intentionally tiny — Plan 09 Task 4's per-node
// supervisor owns the loop that calls Next/Sleep + Reset.
//
// Concurrency: NOT safe for concurrent use. Each supervised connection
// holds its own *Backoff instance.
//
// Default tuning (Base=500ms, Cap=30s) reflects the references doc's
// 'Pragmatic decisions locked' §13: Base * 2^attempt with ±25% jitter,
// capped at Cap, fast on the first retry to recover from transient blips
// without saturating FS during a sustained outage.
type Backoff struct {
	// Base is the initial delay (attempt 0). Zero falls back to 500ms.
	Base time.Duration

	// Cap is the maximum delay regardless of attempt. Zero falls back
	// to 30s.
	Cap time.Duration

	// attempt tracks how many Next() calls have been made since the
	// last Reset(). Unexported because it's part of the state machine,
	// not config.
	attempt int
}

// Next returns the delay for the current attempt and advances the
// counter. The returned duration is min(Cap, Base * 2^attempt) ± 25%
// jitter, where jitter is uniform on [-0.25, +0.25] of the base value.
//
// Jitter prevents thundering-herd reconnects after a coordinated FS
// restart. The math/rand/v2 source is process-seeded — non-secure but
// adequate for backoff (the project's depguard rule bans v1; v2 is the
// modern API).
func (b *Backoff) Next() time.Duration {
	if b.Base <= 0 {
		b.Base = 500 * time.Millisecond
	}
	if b.Cap <= 0 {
		b.Cap = 30 * time.Second
	}

	d := b.Base
	for range b.attempt {
		d *= 2
		if d >= b.Cap {
			d = b.Cap
			break
		}
	}
	if d > b.Cap {
		d = b.Cap
	}

	// Symmetric jitter on [-0.25, +0.25] * d.
	//nolint:gosec // math/rand/v2 is acceptable for non-cryptographic jitter (see depguard rule + plan-09 references doc §13).
	jitter := time.Duration(float64(d) * 0.25 * (rand.Float64()*2 - 1))
	b.attempt++
	return d + jitter
}

// Reset returns the backoff to its initial state — the next Next() call
// will return Base ± jitter again. The supervisor calls this on every
// successful reconnect.
func (b *Backoff) Reset() {
	b.attempt = 0
}

// Sleep blocks for the next backoff duration, returning early when ctx
// is cancelled. Uses time.NewTimer (not time.After) to avoid the leaked-
// timer-per-iteration pitfall flagged by `make grep-time-after`.
//
// Returns ctx.Err() on cancellation, nil otherwise.
func (b *Backoff) Sleep(ctx context.Context) error {
	t := time.NewTimer(b.Next())
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

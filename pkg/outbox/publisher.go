package outbox

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/sociopulse/platform/pkg/eventbus"
)

// Default retry policy values. Kept package-private so callers must use
// the With* options to override.
const (
	defaultPublisherMaxAttempts    = 5
	defaultPublisherInitialBackoff = 50 * time.Millisecond
	defaultPublisherMaxBackoff     = 5 * time.Second
)

// PublisherAdapter bridges the package's internal needs (timeout
// wrapping, retry with jittered exponential backoff) to a plain
// eventbus.Publisher.
//
// Adapter is safe for concurrent use by many goroutines as long as the
// wrapped upstream is. The relay does not currently use this adapter
// itself — the relay's per-row retry is driven by re-drains of the
// outbox table, not in-memory backoff. PublisherAdapter exists for
// callers that want short, in-process resilience around a single
// publish (e.g. a non-outbox audit hook that fires-and-forgets).
type PublisherAdapter struct {
	upstream       eventbus.Publisher
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// PublisherOption tweaks PublisherAdapter behaviour. The functional-
// options pattern keeps the constructor signature stable as we add
// knobs.
type PublisherOption func(*PublisherAdapter)

// WithMaxAttempts overrides the maximum number of publish attempts
// (including the first). Values <= 0 are ignored.
func WithMaxAttempts(n int) PublisherOption {
	return func(a *PublisherAdapter) {
		if n > 0 {
			a.maxAttempts = n
		}
	}
}

// WithBackoff overrides the initial and maximum backoff between
// retries. Values <= 0 are ignored.
func WithBackoff(initial, max time.Duration) PublisherOption {
	return func(a *PublisherAdapter) {
		if initial > 0 {
			a.initialBackoff = initial
		}
		if max > 0 {
			a.maxBackoff = max
		}
	}
}

// NewPublisherAdapter wraps upstream with the relay's timeout/retry
// policy. Pass options to override defaults.
func NewPublisherAdapter(upstream eventbus.Publisher, opts ...PublisherOption) *PublisherAdapter {
	a := &PublisherAdapter{
		upstream:       upstream,
		maxAttempts:    defaultPublisherMaxAttempts,
		initialBackoff: defaultPublisherInitialBackoff,
		maxBackoff:     defaultPublisherMaxBackoff,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Publish forwards the call to upstream after applying the package's
// retry policy. On all-attempts-exhausted, the returned error wraps
// both ErrPublisherFailed (sentinel) and the final upstream error.
//
// Backoff is exponential with full jitter (DynamoDB-style): the wait
// before attempt N is a uniform random value in [0, min(maxBackoff,
// initialBackoff * 2^(N-1))]. Jitter uses crypto/rand because we
// already pull it for other concerns; math/rand would do, but the
// crypto-grade source costs nothing measurable at retry frequency.
func (a *PublisherAdapter) Publish(ctx context.Context, ev Event) error {
	if a.upstream == nil {
		return errors.New("outbox: PublisherAdapter has no upstream")
	}

	// time.NewTimer + Reset in the loop avoids the timer leak per
	// iteration that bites time.After (golang-concurrency § BP6).
	timer := time.NewTimer(0)
	defer timer.Stop()
	if !timer.Stop() {
		<-timer.C
	}

	var lastErr error
	for attempt := 1; attempt <= a.maxAttempts; attempt++ {
		// Honour cancellation up front: an already-cancelled context
		// short-circuits without calling upstream.
		if err := ctx.Err(); err != nil {
			return err
		}

		err := a.upstream.Publish(ctx, ev.Subject, ev.Payload)
		if err == nil {
			return nil
		}
		lastErr = err

		// Don't sleep after the final attempt.
		if attempt == a.maxAttempts {
			break
		}

		wait := a.backoff(attempt)
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			// Drain timer to avoid the leak.
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("%w (last error: %w)", ErrPublisherFailed, lastErr)
}

// backoff returns the wait duration before the (attempt+1)-th try.
// Full-jitter exponential: uniform random in [0, min(maxBackoff, base)].
func (a *PublisherAdapter) backoff(attempt int) time.Duration {
	// base = initial * 2^(attempt-1), capped to maxBackoff.
	base := a.initialBackoff
	for i := 1; i < attempt; i++ {
		base *= 2
		if base >= a.maxBackoff {
			base = a.maxBackoff
			break
		}
	}
	if base <= 0 {
		return 0
	}
	// base is a positive time.Duration (int64); cast to uint64 is safe
	// because we just guarded base > 0 above.
	jitter := secureUint64() % uint64(base) //nolint:gosec // base is positive, see guard above
	return time.Duration(jitter)            //nolint:gosec // jitter < base ≤ maxBackoff (positive)
}

// secureUint64 returns a uniform-random uint64 from crypto/rand. On
// the rare error from rand.Read, we fall back to 0 — that costs us
// jitter on a single retry, never a logical bug.
func secureUint64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b[:])
}

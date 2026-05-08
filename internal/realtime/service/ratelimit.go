package service

import (
	"sync"
	"time"
)

// tokenBucket is an in-memory token-bucket rate limiter used per
// realtime *Connection to clamp inbound frame rate. Production default
// is 100 frames/sec with burst == rate (configurable via
// ConnectionConfig.RateLimitPerSec / RateLimitBurst).
//
// The bucket is intentionally NOT exported: every connection owns one
// privately, the Hub never shares them. Concurrency: safe for a single
// reader goroutine to call Allow concurrently with no other accessors,
// which is the only access pattern in the realtime lifecycle. The
// internal mutex is defence-in-depth for future refactors.
//
// Refill model: lazy. Tokens accumulate on each Allow call based on
// elapsed time since the previous call (capped at burst). This avoids
// a background "refill ticker" goroutine — important for goleak
// hygiene, since each connection already owns a reader / writer /
// pinger triple.
type tokenBucket struct {
	mu        sync.Mutex
	rate      float64 // tokens per second
	burst     float64 // bucket capacity
	tokens    float64 // current token balance
	lastEvent time.Time
	clock     func() time.Time
}

// newTokenBucket constructs a token-bucket. rate is tokens/sec; burst
// is the bucket capacity. clock provides the time source — production
// wires time.Now; tests inject a controllable clock for deterministic
// rate-limit assertions.
//
// rate <= 0 is treated as "unlimited" (Allow always returns true) so
// callers that disable rate-limiting don't have to special-case it.
// burst <= 0 falls back to rate.
func newTokenBucket(rate, burst float64, clock func() time.Time) *tokenBucket {
	if clock == nil {
		clock = time.Now
	}
	if burst <= 0 {
		burst = rate
	}
	b := &tokenBucket{
		rate:      rate,
		burst:     burst,
		clock:     clock,
		lastEvent: clock(),
	}
	if rate > 0 {
		b.tokens = burst
	}
	return b
}

// Allow consumes one token if available. Returns true if the call is
// within budget and the token was deducted; false if the bucket was
// empty (and the caller should reject the action).
//
// Refill is lazy: we add (elapsed * rate) tokens (capped at burst) on
// every call before deciding. This costs O(1) per call and avoids the
// per-bucket background goroutine that a ticker-driven refill would
// require.
func (b *tokenBucket) Allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.rate <= 0 {
		// "Unlimited" bucket. Skip refill bookkeeping entirely.
		return true
	}

	now := b.clock()
	elapsed := now.Sub(b.lastEvent).Seconds()
	if elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed*b.rate)
		b.lastEvent = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Tokens returns the current token balance. Test-only.
func (b *tokenBucket) Tokens() float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}

package esl

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// withinJitter asserts got is in [base*0.75, base*1.25].
func withinJitter(t *testing.T, got, base time.Duration) {
	t.Helper()
	low := time.Duration(float64(base) * 0.75)
	high := time.Duration(float64(base) * 1.25)
	require.GreaterOrEqual(t, got, low, "below jitter band: got=%v base=%v", got, base)
	require.LessOrEqual(t, got, high, "above jitter band: got=%v base=%v", got, base)
}

func TestBackoff_DefaultsApply(t *testing.T) {
	t.Parallel()
	var b Backoff
	d := b.Next()
	// Defaults: Base=500ms → first call returns ~500ms ± 25%.
	withinJitter(t, d, 500*time.Millisecond)
	require.Equal(t, 500*time.Millisecond, b.Base)
	require.Equal(t, 30*time.Second, b.Cap)
}

func TestBackoff_DoublesAcrossAttempts(t *testing.T) {
	t.Parallel()
	// Force a sufficiently large Cap so doubling actually happens.
	b := Backoff{Base: 100 * time.Millisecond, Cap: 10 * time.Second}

	// Run several attempts and assert each is within jitter band of
	// the doubled base.
	expected := []time.Duration{
		100 * time.Millisecond, // attempt 0
		200 * time.Millisecond, // attempt 1
		400 * time.Millisecond, // attempt 2
		800 * time.Millisecond, // attempt 3
		1600 * time.Millisecond,
		3200 * time.Millisecond,
	}
	for _, want := range expected {
		got := b.Next()
		withinJitter(t, got, want)
	}
}

func TestBackoff_CapsAtMax(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: 1 * time.Second, Cap: 4 * time.Second}

	// 1s, 2s, 4s, 4s, 4s, … — should cap at 4s and stay there.
	_ = b.Next() // 1s
	_ = b.Next() // 2s
	_ = b.Next() // 4s (cap)
	for range 5 {
		got := b.Next()
		withinJitter(t, got, 4*time.Second)
	}
}

func TestBackoff_Reset(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: 200 * time.Millisecond, Cap: 5 * time.Second}
	for range 10 {
		_ = b.Next()
	}
	b.Reset()
	require.Equal(t, 0, b.attempt)
	got := b.Next()
	withinJitter(t, got, 200*time.Millisecond)
}

func TestBackoff_SleepReturnsAfterDelay(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: 20 * time.Millisecond, Cap: 100 * time.Millisecond}

	start := time.Now()
	err := b.Sleep(context.Background())
	elapsed := time.Since(start)

	require.NoError(t, err)
	// At least 75% of base (the lower jitter bound).
	require.GreaterOrEqual(t, elapsed, time.Duration(float64(b.Base)*0.75))
}

func TestBackoff_SleepRespectsContextCancel(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: 5 * time.Second, Cap: 30 * time.Second}

	// Cancel the ctx immediately — Sleep must return ctx.Err() within
	// well under the configured Base.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := b.Sleep(ctx)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, elapsed, 200*time.Millisecond,
		"Sleep should return promptly on cancelled ctx, took %v", elapsed)
}

func TestBackoff_SleepRespectsContextDeadline(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: 5 * time.Second, Cap: 30 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := b.Sleep(ctx)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, elapsed, 500*time.Millisecond)
}

func TestBackoff_NoNegativeBase(t *testing.T) {
	t.Parallel()
	// Base set to negative — defaults must repair it.
	b := Backoff{Base: -1, Cap: -1}
	_ = b.Next()
	require.Positive(t, int64(b.Base))
	require.Positive(t, int64(b.Cap))
}

func TestBackoff_JitterIsBounded(t *testing.T) {
	t.Parallel()
	// Statistical sanity: 1000 samples at attempt 0 stay in band.
	for range 1000 {
		b := Backoff{Base: 1 * time.Second, Cap: 10 * time.Second}
		d := b.Next()
		// Lower bound is 0.75 * Base; upper bound is 1.25 * Base.
		require.GreaterOrEqual(t, d, time.Duration(float64(b.Base)*0.75))
		require.LessOrEqual(t, d, time.Duration(float64(b.Base)*1.25))
	}
}

func TestBackoff_DoublingMonotonicMean(t *testing.T) {
	t.Parallel()
	// The mean across many resets at a fixed attempt must approach the
	// doubled base. This guards against off-by-one in the doubling loop.
	const samples = 500
	var sum int64
	for range samples {
		b := Backoff{Base: 100 * time.Millisecond, Cap: 10 * time.Second}
		_ = b.Next() // attempt 0 → ~100ms
		sum += int64(b.Next())
	}
	mean := time.Duration(sum / samples) // ~200ms expected
	diff := math.Abs(float64(mean - 200*time.Millisecond))
	// Allow generous slack — 25% jitter at attempt 1 means ±50ms.
	require.LessOrEqual(t, diff, float64(60*time.Millisecond),
		"mean=%v deviates from 200ms expected", mean)
}

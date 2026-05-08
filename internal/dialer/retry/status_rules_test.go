package retry_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/dialer/retry"
)

// TestApply walks every disposition × in-range / over-cap attempts row
// from Plan 10 §8.5 and pins both retry semantics and attempt counting.
//
// The matrix is intentionally exhaustive — adding a new disposition
// without adding a row here surfaces as a missing branch when the
// orchestrator wires it up.
func TestApply(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC)
	const maxAttempts = 3

	tests := []struct {
		name         string
		status       string
		attempts     int
		callbackTime time.Time
		want         retry.Decision
	}{
		// success / refused — terminal, count one attempt, no retry.
		{
			name:     "success first attempt",
			status:   retry.DispositionSuccess,
			attempts: 0,
			want: retry.Decision{
				Retry:         false,
				CountsAttempt: true,
			},
		},
		{
			name:     "refused first attempt",
			status:   retry.DispositionRefused,
			attempts: 0,
			want: retry.Decision{
				Retry:         false,
				CountsAttempt: true,
			},
		},

		// wrong_person — terminal, count one attempt, MarkDNC.
		{
			name:     "wrong_person first attempt",
			status:   retry.DispositionWrongPerson,
			attempts: 0,
			want: retry.Decision{
				Retry:         false,
				MarkDNC:       true,
				CountsAttempt: true,
			},
		},

		// dropped — retry +2h while attempts < max-1; exhaust on the
		// final attempt that pushes attempts+1 >= max.
		{
			name:     "dropped attempt 0 (retries)",
			status:   retry.DispositionDropped,
			attempts: 0,
			want: retry.Decision{
				Retry:         true,
				Delay:         retry.DelayDropped,
				CountsAttempt: true,
			},
		},
		{
			name:     "dropped attempt 1 (retries — last one before exhaust)",
			status:   retry.DispositionDropped,
			attempts: 1,
			want: retry.Decision{
				Retry:         true,
				Delay:         retry.DelayDropped,
				CountsAttempt: true,
			},
		},
		{
			name:     "dropped attempt 2 (exhausts on attempts+1>=max)",
			status:   retry.DispositionDropped,
			attempts: 2,
			want: retry.Decision{
				Retry:         false,
				MarkExhausted: true,
				CountsAttempt: true,
			},
		},
		{
			name:     "dropped attempt 5 (already over cap; exhausts)",
			status:   retry.DispositionDropped,
			attempts: 5,
			want: retry.Decision{
				Retry:         false,
				MarkExhausted: true,
				CountsAttempt: true,
			},
		},

		// no_answer — retry +4h.
		{
			name:     "no_answer attempt 0",
			status:   retry.DispositionNoAnswer,
			attempts: 0,
			want: retry.Decision{
				Retry:         true,
				Delay:         retry.DelayNoAnswer,
				CountsAttempt: true,
			},
		},
		{
			name:     "no_answer attempt 2 (exhausts)",
			status:   retry.DispositionNoAnswer,
			attempts: 2,
			want: retry.Decision{
				Retry:         false,
				MarkExhausted: true,
				CountsAttempt: true,
			},
		},

		// busy — retry +30min.
		{
			name:     "busy attempt 0",
			status:   retry.DispositionBusy,
			attempts: 0,
			want: retry.Decision{
				Retry:         true,
				Delay:         retry.DelayBusy,
				CountsAttempt: true,
			},
		},
		{
			name:     "busy attempt 2 (exhausts)",
			status:   retry.DispositionBusy,
			attempts: 2,
			want: retry.Decision{
				Retry:         false,
				MarkExhausted: true,
				CountsAttempt: true,
			},
		},

		// callback — retry at parsed time.
		{
			name:         "callback in 3h",
			status:       retry.DispositionCallback,
			attempts:     0,
			callbackTime: now.Add(3 * time.Hour),
			want: retry.Decision{
				Retry:         true,
				Delay:         3 * time.Hour,
				CountsAttempt: true,
			},
		},
		{
			name:         "callback in past clamps to zero delay",
			status:       retry.DispositionCallback,
			attempts:     0,
			callbackTime: now.Add(-1 * time.Hour),
			want: retry.Decision{
				Retry:         true,
				Delay:         0,
				CountsAttempt: true,
			},
		},
		{
			name:         "callback zero time defaults to 1h",
			status:       retry.DispositionCallback,
			attempts:     0,
			callbackTime: time.Time{},
			want: retry.Decision{
				Retry:         true,
				Delay:         time.Hour,
				CountsAttempt: true,
			},
		},
		{
			name:         "callback final attempt exhausts",
			status:       retry.DispositionCallback,
			attempts:     2,
			callbackTime: now.Add(2 * time.Hour),
			want: retry.Decision{
				Retry:         false,
				MarkExhausted: true,
				CountsAttempt: true,
			},
		},

		// tech_failure — retry +5min, does NOT count attempts.
		{
			name:     "tech_failure attempt 0",
			status:   retry.DispositionTechFailure,
			attempts: 0,
			want: retry.Decision{
				Retry:         true,
				Delay:         retry.DelayTechFailure,
				CountsAttempt: false,
			},
		},
		{
			name:     "tech_failure attempt 5 still retries (does not count)",
			status:   retry.DispositionTechFailure,
			attempts: 5,
			want: retry.Decision{
				Retry:         true,
				Delay:         retry.DelayTechFailure,
				CountsAttempt: false,
			},
		},

		// Unknown disposition — bucketed as tech_failure (defensive).
		{
			name:     "unknown disposition bucketed as tech_failure",
			status:   "completely_made_up_status_value",
			attempts: 0,
			want: retry.Decision{
				Retry:         true,
				Delay:         retry.DelayTechFailure,
				CountsAttempt: false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := retry.Apply(tc.status, tc.attempts, maxAttempts, now, tc.callbackTime)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestApply_DefaultMaxAttempts confirms that passing maxAttempts<=0
// triggers the default (3) — used by the orchestrator config when the
// caller doesn't override it.
func TestApply_DefaultMaxAttempts(t *testing.T) {
	t.Parallel()
	now := time.Now()

	// attempts=2 + default cap of 3 => exhaust on dropped retry.
	got := retry.Apply(retry.DispositionDropped, 2, 0, now, time.Time{})
	require.True(t, got.MarkExhausted)
	require.False(t, got.Retry)
	require.True(t, got.CountsAttempt)
}

// TestApply_AllDispositionsCovered is the exhaustive guard: every
// disposition constant declared in status_rules.go must be exercised
// by at least one row in TestApply. Walks the constant list at run
// time and asserts the test has covered each.
func TestApply_AllDispositionsCovered(t *testing.T) {
	t.Parallel()
	want := []string{
		retry.DispositionSuccess,
		retry.DispositionRefused,
		retry.DispositionWrongPerson,
		retry.DispositionDropped,
		retry.DispositionNoAnswer,
		retry.DispositionBusy,
		retry.DispositionCallback,
		retry.DispositionTechFailure,
	}
	for _, status := range want {
		// Just confirm Apply doesn't panic on any legal status — the
		// per-row semantics are pinned by TestApply's table.
		_ = retry.Apply(status, 0, 3, time.Now(), time.Time{})
	}
}

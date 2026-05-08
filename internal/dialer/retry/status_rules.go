package retry

import "time"

// Disposition is the call status string the operator submits via the
// SubmitStatus FSM event. The retry orchestrator consumes the same
// vocabulary; the constants below are the EXHAUSTIVE legal set.
//
// Adding a new disposition requires:
//
//  1. A new constant here.
//  2. A row in Apply()'s switch.
//  3. A test row in status_rules_test.go.
//
// The exhaustive switch is enforced at test time (see TestApply_AllDispositions
// in status_rules_test.go).
const (
	// DispositionSuccess — interview completed; respondent answered every
	// required question. No retry; counts as one attempt.
	DispositionSuccess = "success"
	// DispositionRefused — respondent picked up but declined to participate.
	// Terminal: no retry, counts as one attempt. Per Plan 10 §8.5 a refusal
	// does NOT auto-DNC; that's a separate operator action.
	DispositionRefused = "refused"
	// DispositionWrongPerson — operator confirmed the dialed number does
	// not belong to the intended respondent (e.g. wrong household). Adds
	// the phone to DNC + counts as one attempt; no retry.
	DispositionWrongPerson = "wrong_person"
	// DispositionDropped — call was answered but the line dropped before
	// the survey completed. Retry +2h.
	DispositionDropped = "dropped"
	// DispositionNoAnswer — phone rang but no one picked up. Retry +4h.
	DispositionNoAnswer = "no_answer"
	// DispositionBusy — phone returned a busy signal. Retry +30min.
	DispositionBusy = "busy"
	// DispositionCallback — respondent asked to be called back at a
	// specific time. Retry at the parsed callback time (caller supplies
	// it via Apply's callbackTime parameter); attempts is bumped.
	DispositionCallback = "callback"
	// DispositionTechFailure — bridge / FreeSWITCH / network error
	// prevented the call from being placed or completed. Retry +5min;
	// does NOT count toward attempts (the survey didn't actually run).
	DispositionTechFailure = "tech_failure"
)

// Default delays per Plan 10 §8.5. Exposed as named values so tests
// pin against the constants rather than magic numbers.
const (
	DelayDropped     = 2 * time.Hour
	DelayNoAnswer    = 4 * time.Hour
	DelayBusy        = 30 * time.Minute
	DelayTechFailure = 5 * time.Minute
)

// DefaultMaxAttempts is the cap before a respondent is marked exhausted
// (Plan 10 §8.5).
const DefaultMaxAttempts = 3

// Decision is the result of Apply(): a self-contained instruction set
// the orchestrator follows to update the respondents row + (optionally)
// re-enqueue.
//
// All booleans default to zero-value false; a status that doesn't retry
// returns Decision{Retry: false} and the orchestrator does no enqueue.
type Decision struct {
	// Retry indicates the orchestrator must re-enqueue the respondent
	// after Delay elapses. When false, no enqueue happens; the
	// respondent's terminal-or-deferred state is captured by
	// MarkExhausted / MarkDNC.
	Retry bool
	// Delay is the wall-clock duration to defer the retry. For
	// DispositionCallback this is callbackTime - now (clamped to 0 for
	// past times). Zero for non-retry decisions.
	Delay time.Duration
	// MarkExhausted forces respondents.status='exhausted' regardless of
	// Retry — set when attempts+1 >= maxAttempts on a status that would
	// otherwise retry. The orchestrator skips the enqueue in this case.
	MarkExhausted bool
	// MarkDNC instructs the orchestrator to add the phone to project DNC.
	// Today only DispositionWrongPerson sets this; future statuses may.
	// The DNC add is a separate concern from MarkExhausted — both can be
	// true (a wrong_person on the final attempt) but in practice
	// wrong_person is terminal regardless of attempts.
	MarkDNC bool
	// CountsAttempt indicates whether the disposition increments the
	// respondent's attempts counter. False for tech_failure (the survey
	// didn't actually run); true for everything else.
	CountsAttempt bool
}

// Apply maps a (status, attempts, maxAttempts, callbackTime) tuple to a
// Decision per the Plan 10 §8.5 retry rules:
//
//	status         | retry?     | delay      | counts toward attempts?
//	---------------|------------|------------|------------------------
//	success        | NO         | -          | yes (1)
//	refused        | NO         | -          | yes (1)
//	wrong_person   | NO + DNC   | -          | yes (1) + DNC mark
//	dropped        | yes        | +2h        | yes
//	no_answer      | yes        | +4h        | yes
//	busy           | yes        | +30min     | yes
//	callback       | yes        | parsed_dt  | yes (treat as scheduled)
//	tech_failure   | yes        | +5min      | NOT counted
//
// `attempts` is the count BEFORE this disposition is applied. The
// orchestrator pre-bumps the counter by 1 for the new attempt (when
// CountsAttempt is true) before consulting MarkExhausted.
//
// `now` is the reference time for callback-delay computation. Callers
// supply the orchestrator's clock so tests are deterministic.
//
// `callbackTime` is consulted ONLY for status==callback; ignored
// otherwise. A zero callbackTime on a callback status is treated as
// "callback in 1h" — a defensive default that prevents a parser bug
// from re-firing a callback immediately.
//
// Unknown status strings are bucketed as tech_failure: the orchestrator
// retries cautiously rather than abandoning the row. (Plan 10 §8.5 does
// not mandate a behaviour for unknown values; we choose the safer side.)
func Apply(status string, attempts, maxAttempts int, now, callbackTime time.Time) Decision {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}

	switch status {
	case DispositionSuccess, DispositionRefused:
		return Decision{
			Retry:         false,
			CountsAttempt: true,
		}
	case DispositionWrongPerson:
		return Decision{
			Retry:         false,
			MarkDNC:       true,
			CountsAttempt: true,
		}
	case DispositionDropped:
		return retryWithCap(attempts, maxAttempts, DelayDropped)
	case DispositionNoAnswer:
		return retryWithCap(attempts, maxAttempts, DelayNoAnswer)
	case DispositionBusy:
		return retryWithCap(attempts, maxAttempts, DelayBusy)
	case DispositionCallback:
		delay := callbackDelay(now, callbackTime)
		return retryWithCap(attempts, maxAttempts, delay)
	case DispositionTechFailure:
		// tech_failure does NOT count toward attempts; the cap check is
		// therefore independent of attempts.
		return Decision{
			Retry:         true,
			Delay:         DelayTechFailure,
			CountsAttempt: false,
		}
	default:
		// Unknown status — treat as tech_failure (safer default than
		// abandoning the row). The bucket keeps the orchestrator
		// progressing while ops triages the surfaced metric.
		return Decision{
			Retry:         true,
			Delay:         DelayTechFailure,
			CountsAttempt: false,
		}
	}
}

// retryWithCap returns the retry decision for a counts-toward-attempts
// disposition: when attempts+1 (the post-increment value) would meet or
// exceed maxAttempts, the row is exhausted and no retry fires.
func retryWithCap(attempts, maxAttempts int, delay time.Duration) Decision {
	if attempts+1 >= maxAttempts {
		// Final attempt landed on a retry-able status; row is exhausted.
		// Still counts the attempt — the row's audit trail records the
		// final disposition that pushed it over the line.
		return Decision{
			Retry:         false,
			MarkExhausted: true,
			CountsAttempt: true,
		}
	}
	return Decision{
		Retry:         true,
		Delay:         delay,
		CountsAttempt: true,
	}
}

// callbackDelay computes the delay for a callback disposition. A
// callback in the past (clock skew, late operator submission) clamps to
// zero so the orchestrator re-enqueues immediately. A zero
// callbackTime falls back to a 1h default per the Apply doc.
func callbackDelay(now, callbackTime time.Time) time.Duration {
	if callbackTime.IsZero() {
		return time.Hour
	}
	d := callbackTime.Sub(now)
	return max(d, 0)
}

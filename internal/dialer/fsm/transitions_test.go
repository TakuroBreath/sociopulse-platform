package fsm

import (
	"errors"
	"testing"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// TestTransitions_FullMatrix enumerates every (state, event) pair the FSM
// must classify. Per CANONICAL CONTEXT.md spec:
//
//	offline → ready → dialing → call → status → verify → ready
//	plus `pause` from any non-offline / non-pause state
//	verify is reachable ONLY from status with a success-class outcome
//
// The matrix is the audit-grade source of truth for what is allowed and
// what is rejected — a future agent who silently flips a cell here would
// re-introduce the spec drift this task fixes. For status→verify the
// row asserts the outcome-gated behaviour: the resolveTransition helper
// is the single decision point so callers cannot bypass the guard.
//
// 7 states × 12 events = 84 entries. Each row carries the wantTarget
// state (zero value = expect ErrInvalidTransition).
func TestTransitions_FullMatrix(t *testing.T) {
	t.Parallel()
	type row struct {
		from    api.State
		event   api.Event
		outcome api.StatusOutcome // only consulted for (status, go_verify)
		want    api.State         // zero value → expect ErrInvalidTransition
	}
	// blank api.State => expect ErrInvalidTransition (sentinel zero value).
	const reject api.State = ""

	rows := []row{
		// ─── offline (7×12 row 1) ─────────────────────────────────────
		{api.StateOffline, api.EventStartShift, OutcomeUnknown, api.StateReady},
		{api.StateOffline, api.EventEndShift, OutcomeUnknown, reject},
		{api.StateOffline, api.EventGoReady, OutcomeUnknown, reject},
		{api.StateOffline, api.EventGoPause, OutcomeUnknown, reject},
		{api.StateOffline, api.EventResume, OutcomeUnknown, reject},
		{api.StateOffline, api.EventCallStarted, OutcomeUnknown, reject},
		{api.StateOffline, api.EventCallEnded, OutcomeUnknown, reject},
		{api.StateOffline, api.EventCallFailed, OutcomeUnknown, reject},
		{api.StateOffline, api.EventStatusSubmitted, OutcomeUnknown, reject},
		{api.StateOffline, api.EventGoVerify, OutcomeUnknown, reject},
		{api.StateOffline, api.EventVerifyDone, OutcomeUnknown, reject},
		{api.StateOffline, api.EventForceOffline, OutcomeUnknown, reject}, // Force bypasses the table

		// ─── ready ─────────────────────────────────────────────────────
		{api.StateReady, api.EventStartShift, OutcomeUnknown, reject},
		{api.StateReady, api.EventEndShift, OutcomeUnknown, api.StateOffline},
		{api.StateReady, api.EventGoReady, OutcomeUnknown, reject},
		{api.StateReady, api.EventGoPause, OutcomeUnknown, api.StatePause},
		{api.StateReady, api.EventResume, OutcomeUnknown, reject},
		{api.StateReady, api.EventCallStarted, OutcomeUnknown, api.StateDialing},
		{api.StateReady, api.EventCallEnded, OutcomeUnknown, reject},
		{api.StateReady, api.EventCallFailed, OutcomeUnknown, reject},
		{api.StateReady, api.EventStatusSubmitted, OutcomeUnknown, reject},
		{api.StateReady, api.EventGoVerify, OutcomeUnknown, reject}, // SPEC: verify only from status
		{api.StateReady, api.EventVerifyDone, OutcomeUnknown, reject},
		{api.StateReady, api.EventForceOffline, OutcomeUnknown, reject},

		// ─── dialing ───────────────────────────────────────────────────
		{api.StateDialing, api.EventStartShift, OutcomeUnknown, reject},
		{api.StateDialing, api.EventEndShift, OutcomeUnknown, reject},
		{api.StateDialing, api.EventGoReady, OutcomeUnknown, reject},
		{api.StateDialing, api.EventGoPause, OutcomeUnknown, api.StatePause}, // SPEC: pause from any non-offline
		{api.StateDialing, api.EventResume, OutcomeUnknown, reject},
		{api.StateDialing, api.EventCallStarted, OutcomeUnknown, api.StateCall},
		{api.StateDialing, api.EventCallEnded, OutcomeUnknown, api.StateStatus},
		{api.StateDialing, api.EventCallFailed, OutcomeUnknown, api.StateStatus},
		{api.StateDialing, api.EventStatusSubmitted, OutcomeUnknown, reject},
		{api.StateDialing, api.EventGoVerify, OutcomeUnknown, reject},
		{api.StateDialing, api.EventVerifyDone, OutcomeUnknown, reject},
		{api.StateDialing, api.EventForceOffline, OutcomeUnknown, reject},

		// ─── call ──────────────────────────────────────────────────────
		{api.StateCall, api.EventStartShift, OutcomeUnknown, reject},
		{api.StateCall, api.EventEndShift, OutcomeUnknown, reject},
		{api.StateCall, api.EventGoReady, OutcomeUnknown, reject},
		{api.StateCall, api.EventGoPause, OutcomeUnknown, api.StatePause}, // SPEC: pause from any non-offline
		{api.StateCall, api.EventResume, OutcomeUnknown, reject},
		{api.StateCall, api.EventCallStarted, OutcomeUnknown, reject},
		{api.StateCall, api.EventCallEnded, OutcomeUnknown, api.StateStatus},
		{api.StateCall, api.EventCallFailed, OutcomeUnknown, reject},
		{api.StateCall, api.EventStatusSubmitted, OutcomeUnknown, reject},
		{api.StateCall, api.EventGoVerify, OutcomeUnknown, reject},
		{api.StateCall, api.EventVerifyDone, OutcomeUnknown, reject},
		{api.StateCall, api.EventForceOffline, OutcomeUnknown, reject},

		// ─── status (outcome-bearing) ─────────────────────────────────
		{api.StateStatus, api.EventStartShift, OutcomeUnknown, reject},
		{api.StateStatus, api.EventEndShift, OutcomeUnknown, api.StateOffline}, // graceful end after wrap-up
		{api.StateStatus, api.EventGoReady, OutcomeUnknown, reject},
		{api.StateStatus, api.EventGoPause, OutcomeUnknown, api.StatePause}, // SPEC: pause from any non-offline
		{api.StateStatus, api.EventResume, OutcomeUnknown, reject},
		{api.StateStatus, api.EventCallStarted, OutcomeUnknown, reject},
		{api.StateStatus, api.EventCallEnded, OutcomeUnknown, reject},
		{api.StateStatus, api.EventCallFailed, OutcomeUnknown, reject},
		{api.StateStatus, api.EventStatusSubmitted, OutcomeUnknown, api.StateReady},
		{api.StateStatus, api.EventGoVerify, api.OutcomeSuccess, api.StateVerify}, // SPEC: success-class only
		{api.StateStatus, api.EventGoVerify, api.OutcomeNoAnswer, reject},         // non-success → reject
		{api.StateStatus, api.EventGoVerify, api.OutcomeBusy, reject},             // non-success → reject
		{api.StateStatus, api.EventGoVerify, api.OutcomeWrongPerson, reject},      // non-success → reject
		{api.StateStatus, api.EventGoVerify, api.OutcomeDNCHit, reject},           // non-success → reject
		{api.StateStatus, api.EventGoVerify, api.OutcomeTechFailure, reject},      // non-success → reject
		{api.StateStatus, api.EventGoVerify, OutcomeUnknown, reject},              // unset → fail-loud
		{api.StateStatus, api.EventVerifyDone, OutcomeUnknown, reject},
		{api.StateStatus, api.EventForceOffline, OutcomeUnknown, reject},

		// ─── verify ────────────────────────────────────────────────────
		{api.StateVerify, api.EventStartShift, OutcomeUnknown, reject},
		{api.StateVerify, api.EventEndShift, OutcomeUnknown, reject},
		{api.StateVerify, api.EventGoReady, OutcomeUnknown, reject},
		{api.StateVerify, api.EventGoPause, OutcomeUnknown, api.StatePause}, // SPEC: pause from any non-offline
		{api.StateVerify, api.EventResume, OutcomeUnknown, reject},
		{api.StateVerify, api.EventCallStarted, OutcomeUnknown, reject},
		{api.StateVerify, api.EventCallEnded, OutcomeUnknown, reject},
		{api.StateVerify, api.EventCallFailed, OutcomeUnknown, reject},
		{api.StateVerify, api.EventStatusSubmitted, OutcomeUnknown, reject},
		{api.StateVerify, api.EventGoVerify, OutcomeUnknown, reject},
		{api.StateVerify, api.EventVerifyDone, OutcomeUnknown, api.StateReady},
		{api.StateVerify, api.EventForceOffline, OutcomeUnknown, reject},

		// ─── pause ─────────────────────────────────────────────────────
		{api.StatePause, api.EventStartShift, OutcomeUnknown, reject},
		{api.StatePause, api.EventEndShift, OutcomeUnknown, api.StateOffline},
		{api.StatePause, api.EventGoReady, OutcomeUnknown, reject},
		{api.StatePause, api.EventGoPause, OutcomeUnknown, reject}, // already paused — fail-loud
		{api.StatePause, api.EventResume, OutcomeUnknown, api.StateReady},
		{api.StatePause, api.EventCallStarted, OutcomeUnknown, reject},
		{api.StatePause, api.EventCallEnded, OutcomeUnknown, reject},
		{api.StatePause, api.EventCallFailed, OutcomeUnknown, reject},
		{api.StatePause, api.EventStatusSubmitted, OutcomeUnknown, reject},
		{api.StatePause, api.EventGoVerify, OutcomeUnknown, reject},
		{api.StatePause, api.EventVerifyDone, OutcomeUnknown, reject},
		{api.StatePause, api.EventForceOffline, OutcomeUnknown, reject},
	}
	for _, tc := range rows {
		t.Run(string(tc.from)+"_"+string(tc.event)+"_"+string(tc.outcome), func(t *testing.T) {
			t.Parallel()
			got, err := resolveTransition(tc.from, tc.event, tc.outcome)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("(%s, %s, outcome=%s): want ErrInvalidTransition, got next=%s, nil err",
						tc.from, tc.event, tc.outcome, got)
				}
				if !errors.Is(err, api.ErrInvalidTransition) {
					t.Fatalf("(%s, %s, outcome=%s): want ErrInvalidTransition wrapping, got %v",
						tc.from, tc.event, tc.outcome, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("(%s, %s, outcome=%s): want target=%s, got err=%v",
					tc.from, tc.event, tc.outcome, tc.want, err)
			}
			if got != tc.want {
				t.Fatalf("(%s, %s, outcome=%s): want %s, got %s",
					tc.from, tc.event, tc.outcome, tc.want, got)
			}
		})
	}
}

// TestTransitions_VerifyOnlyReachableFromSuccessOutcome focuses the
// outcome guard for go_verify. CONTEXT.md: "verify is reachable only
// from success-class outcomes". The previous implementation routed
// (ready, go_verify) → verify, which violated the spec. The fix is
// twofold: the only (state, event) → verify edge is (status, go_verify),
// and it is gated by the operator's carried StatusOutcome.
func TestTransitions_VerifyOnlyReachableFromSuccessOutcome(t *testing.T) {
	t.Parallel()
	t.Run("status_with_OutcomeSuccess_transitions_to_verify", func(t *testing.T) {
		t.Parallel()
		got, err := resolveTransition(api.StateStatus, api.EventGoVerify, api.OutcomeSuccess)
		if err != nil {
			t.Fatalf("status+success → verify: want nil err, got %v", err)
		}
		if got != api.StateVerify {
			t.Fatalf("status+success → verify: want %s, got %s", api.StateVerify, got)
		}
	})

	t.Run("status_with_OutcomeNoAnswer_rejects", func(t *testing.T) {
		t.Parallel()
		_, err := resolveTransition(api.StateStatus, api.EventGoVerify, api.OutcomeNoAnswer)
		if !errors.Is(err, api.ErrInvalidTransition) {
			t.Fatalf("status+no_answer → verify: want ErrInvalidTransition, got %v", err)
		}
	})

	t.Run("status_with_OutcomeBusy_rejects", func(t *testing.T) {
		t.Parallel()
		_, err := resolveTransition(api.StateStatus, api.EventGoVerify, api.OutcomeBusy)
		if !errors.Is(err, api.ErrInvalidTransition) {
			t.Fatalf("status+busy → verify: want ErrInvalidTransition, got %v", err)
		}
	})

	t.Run("status_with_OutcomeWrongPerson_rejects", func(t *testing.T) {
		t.Parallel()
		_, err := resolveTransition(api.StateStatus, api.EventGoVerify, api.OutcomeWrongPerson)
		if !errors.Is(err, api.ErrInvalidTransition) {
			t.Fatalf("status+wrong_person → verify: want ErrInvalidTransition, got %v", err)
		}
	})

	t.Run("status_with_OutcomeDNCHit_rejects", func(t *testing.T) {
		t.Parallel()
		_, err := resolveTransition(api.StateStatus, api.EventGoVerify, api.OutcomeDNCHit)
		if !errors.Is(err, api.ErrInvalidTransition) {
			t.Fatalf("status+dnc_hit → verify: want ErrInvalidTransition, got %v", err)
		}
	})

	t.Run("status_with_OutcomeTechFailure_rejects", func(t *testing.T) {
		t.Parallel()
		_, err := resolveTransition(api.StateStatus, api.EventGoVerify, api.OutcomeTechFailure)
		if !errors.Is(err, api.ErrInvalidTransition) {
			t.Fatalf("status+tech_failure → verify: want ErrInvalidTransition, got %v", err)
		}
	})

	t.Run("status_with_OutcomeUnknown_rejects", func(t *testing.T) {
		t.Parallel()
		_, err := resolveTransition(api.StateStatus, api.EventGoVerify, OutcomeUnknown)
		if !errors.Is(err, api.ErrInvalidTransition) {
			t.Fatalf("status+unknown → verify: want ErrInvalidTransition, got %v", err)
		}
	})

	t.Run("ready_with_OutcomeSuccess_still_rejects", func(t *testing.T) {
		t.Parallel()
		// Even if a caller set a stale Outcome on a ready state, verify
		// is still rejected — the edge does not exist from ready.
		_, err := resolveTransition(api.StateReady, api.EventGoVerify, api.OutcomeSuccess)
		if !errors.Is(err, api.ErrInvalidTransition) {
			t.Fatalf("ready+success → verify: want ErrInvalidTransition, got %v", err)
		}
	})
}

// TestTransitions_PauseFromAllNonOfflineStates exercises the
// CONTEXT.md "pause from any state" invariant. The previous
// implementation only allowed pause from ready, which violated the
// operator panic-pause user feature. Pause from offline / pause itself
// is rejected fail-loud (no-op would mask programmer errors).
func TestTransitions_PauseFromAllNonOfflineStates(t *testing.T) {
	t.Parallel()
	t.Run("ready_to_pause", func(t *testing.T) {
		t.Parallel()
		got, err := resolveTransition(api.StateReady, api.EventGoPause, OutcomeUnknown)
		if err != nil || got != api.StatePause {
			t.Fatalf("ready → pause: want %s nil, got %s %v", api.StatePause, got, err)
		}
	})
	t.Run("dialing_to_pause", func(t *testing.T) {
		t.Parallel()
		got, err := resolveTransition(api.StateDialing, api.EventGoPause, OutcomeUnknown)
		if err != nil || got != api.StatePause {
			t.Fatalf("dialing → pause: want %s nil, got %s %v", api.StatePause, got, err)
		}
	})
	t.Run("call_to_pause", func(t *testing.T) {
		t.Parallel()
		got, err := resolveTransition(api.StateCall, api.EventGoPause, OutcomeUnknown)
		if err != nil || got != api.StatePause {
			t.Fatalf("call → pause: want %s nil, got %s %v", api.StatePause, got, err)
		}
	})
	t.Run("status_to_pause", func(t *testing.T) {
		t.Parallel()
		got, err := resolveTransition(api.StateStatus, api.EventGoPause, OutcomeUnknown)
		if err != nil || got != api.StatePause {
			t.Fatalf("status → pause: want %s nil, got %s %v", api.StatePause, got, err)
		}
	})
	t.Run("verify_to_pause", func(t *testing.T) {
		t.Parallel()
		got, err := resolveTransition(api.StateVerify, api.EventGoPause, OutcomeUnknown)
		if err != nil || got != api.StatePause {
			t.Fatalf("verify → pause: want %s nil, got %s %v", api.StatePause, got, err)
		}
	})
	t.Run("offline_rejects_pause", func(t *testing.T) {
		t.Parallel()
		_, err := resolveTransition(api.StateOffline, api.EventGoPause, OutcomeUnknown)
		if !errors.Is(err, api.ErrInvalidTransition) {
			t.Fatalf("offline → pause: want ErrInvalidTransition, got %v", err)
		}
	})
	t.Run("pause_rejects_pause", func(t *testing.T) {
		t.Parallel()
		// Already paused — fail-loud so a double-press is caught.
		_, err := resolveTransition(api.StatePause, api.EventGoPause, OutcomeUnknown)
		if !errors.Is(err, api.ErrInvalidTransition) {
			t.Fatalf("pause → pause: want ErrInvalidTransition, got %v", err)
		}
	})
}

// TestIsTerminal asserts the documented terminal states. Today only
// offline qualifies — all other states have at least one outgoing edge.
func TestIsTerminal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state api.State
		want  bool
	}{
		{api.StateOffline, true},
		{api.StateReady, false},
		{api.StateDialing, false},
		{api.StateCall, false},
		{api.StateStatus, false},
		{api.StateVerify, false},
		{api.StatePause, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()
			if got := IsTerminal(tc.state); got != tc.want {
				t.Fatalf("IsTerminal(%s): want %v, got %v", tc.state, tc.want, got)
			}
		})
	}
}

// TestTransitions_AllRecognizedStatesAndEvents asserts the keys of the
// transitions map are recognized api.State / api.Event values. A typo in
// a constant would make IsValid() reject it — this catches such typos at
// compile-test time rather than at first runtime use.
func TestTransitions_AllRecognizedStatesAndEvents(t *testing.T) {
	t.Parallel()
	for e, next := range transitions {
		if !e.from.Valid() {
			t.Errorf("transitions key has unrecognized State: %q", e.from)
		}
		if !e.event.Valid() {
			t.Errorf("transitions key has unrecognized Event: %q", e.event)
		}
		if !next.Valid() {
			t.Errorf("transitions value has unrecognized State: %q", next)
		}
	}
}

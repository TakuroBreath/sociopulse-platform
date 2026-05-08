package fsm

import (
	"testing"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// TestTransitions_KnownEdges asserts the documented (state, event) pairs
// each map to the expected next state. Adding a new edge requires
// extending this list — the dialer's HTTP layer relies on the exact set
// of valid transitions; an accidental rename in transitions.go that
// flips one cell would silently break a workflow.
func TestTransitions_KnownEdges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from  api.State
		event api.Event
		want  api.State
	}{
		// Shift lifecycle
		{api.StateOffline, api.EventStartShift, api.StateReady},
		{api.StateReady, api.EventEndShift, api.StateOffline},
		{api.StatePause, api.EventEndShift, api.StateOffline},
		{api.StateStatus, api.EventEndShift, api.StateOffline},

		// Pause / resume / go_ready alias
		{api.StateReady, api.EventGoPause, api.StatePause},
		{api.StatePause, api.EventResume, api.StateReady},
		{api.StatePause, api.EventGoReady, api.StateReady},

		// Dial → answer → call → status
		{api.StateReady, api.EventCallStarted, api.StateDialing},
		{api.StateDialing, api.EventCallStarted, api.StateCall},
		{api.StateDialing, api.EventCallEnded, api.StateReady},
		{api.StateDialing, api.EventCallFailed, api.StateReady},
		{api.StateCall, api.EventCallEnded, api.StateStatus},

		// Status submission
		{api.StateStatus, api.EventStatusSubmitted, api.StateReady},

		// Verify
		{api.StateStatus, api.EventGoVerify, api.StateVerify},
		{api.StateVerify, api.EventVerifyDone, api.StateReady},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"_"+string(tc.event), func(t *testing.T) {
			t.Parallel()
			got, ok := transitions[edge{from: tc.from, event: tc.event}]
			if !ok {
				t.Fatalf("(%s, %s): edge missing from transitions table", tc.from, tc.event)
			}
			if got != tc.want {
				t.Fatalf("(%s, %s): want %s, got %s", tc.from, tc.event, tc.want, got)
			}
		})
	}
}

// TestTransitions_RejectsInvalid covers a handful of edges that MUST NOT
// appear in the transitions map. Listing every invalid pair is brittle
// (pair count = states × events = 84) — instead we hand-pick a set of
// "looks plausible but is wrong" edges that have caused bugs in similar
// FSMs before.
func TestTransitions_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from  api.State
		event api.Event
	}{
		// Wrong-direction shift transitions
		{api.StateOffline, api.EventEndShift},      // already offline
		{api.StateOffline, api.EventGoPause},       // can't pause when not on shift
		{api.StateReady, api.EventStartShift},      // already on shift
		{api.StateCall, api.EventStartShift},       // mid-call → start_shift is meaningless
		{api.StateCall, api.EventEndShift},         // can't end shift mid-call (use Force)
		{api.StateDialing, api.EventEndShift},      // dialing — not a graceful end point
		{api.StateVerify, api.EventEndShift},       // verify must complete first
		{api.StatePause, api.EventCallStarted},     // calls only start from ready
		{api.StateOffline, api.EventCallStarted},   // ditto
		{api.StateCall, api.EventCallStarted},      // already on a call
		{api.StateReady, api.EventCallEnded},       // no in-flight call to end
		{api.StateReady, api.EventStatusSubmitted}, // wrong-state submit
		{api.StateCall, api.EventStatusSubmitted},  // must end call first
		{api.StateCall, api.EventGoPause},          // can't pause mid-call
		{api.StateCall, api.EventGoVerify},         // can't verify mid-call
		{api.StateReady, api.EventGoVerify},        // verify is reached via status
		{api.StateReady, api.EventVerifyDone},      // not in verify
		{api.StateOffline, api.EventResume},        // can't resume when offline
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"_"+string(tc.event), func(t *testing.T) {
			t.Parallel()
			if _, ok := transitions[edge{from: tc.from, event: tc.event}]; ok {
				t.Fatalf("(%s, %s): edge unexpectedly present in transitions table", tc.from, tc.event)
			}
		})
	}
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

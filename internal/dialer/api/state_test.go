// state_test.go — exhaustive coverage for State.Valid() and Event.Valid()
// helpers. These are the recognized-string predicates used by the FSM impl
// (internal/dialer/fsm) before applying a transition.
//
// Why exhaustive:
//   - The FSM's Force() escape hatch accepts a State argument from operator
//     UI / supervisor commands; Valid() is the gate that rejects garbage
//     before we touch Redis. A broken predicate here lets corrupt data
//     into the source-of-truth hash.
//   - The store's parseHash uses State.Valid() to detect a corrupt row
//     after deserialization. Same severity.
//
// We test every documented enum value plus the zero value plus a
// hand-picked set of "looks similar but isn't" strings so a future rename
// of an internal constant is caught here, not in production.
package api_test

import (
	"testing"

	"github.com/sociopulse/platform/internal/dialer/api"
)

func TestStateValid_KnownValues(t *testing.T) {
	t.Parallel()
	cases := []api.State{
		api.StateOffline,
		api.StateReady,
		api.StateDialing,
		api.StateCall,
		api.StateStatus,
		api.StateVerify,
		api.StatePause,
	}
	for _, s := range cases {
		t.Run(string(s), func(t *testing.T) {
			t.Parallel()
			if !s.Valid() {
				t.Fatalf("%q: want Valid() true, got false", s)
			}
		})
	}
}

func TestStateValid_RejectsUnknown(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",          // zero value
		"OFFLINE",   // wrong case
		"ready ",    // trailing space
		"unknown",   // garbage
		"status_v2", // typo
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if api.State(raw).Valid() {
				t.Fatalf("%q: want Valid() false, got true", raw)
			}
		})
	}
}

func TestEventValid_KnownValues(t *testing.T) {
	t.Parallel()
	cases := []api.Event{
		api.EventStartShift,
		api.EventEndShift,
		api.EventGoReady,
		api.EventGoPause,
		api.EventResume,
		api.EventCallStarted,
		api.EventCallEnded,
		api.EventCallFailed,
		api.EventStatusSubmitted,
		api.EventGoVerify,
		api.EventVerifyDone,
		api.EventForceOffline,
	}
	for _, e := range cases {
		t.Run(string(e), func(t *testing.T) {
			t.Parallel()
			if !e.Valid() {
				t.Fatalf("%q: want Valid() true, got false", e)
			}
		})
	}
}

func TestEventValid_RejectsUnknown(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",              // zero value
		"START_SHIFT",   // wrong case
		"go_ready ",     // trailing space
		"unknown_event", // garbage
		"call_start",    // typo (should be call_started)
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if api.Event(raw).Valid() {
				t.Fatalf("%q: want Valid() false, got true", raw)
			}
		})
	}
}

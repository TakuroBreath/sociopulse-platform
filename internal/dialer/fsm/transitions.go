package fsm

import "github.com/sociopulse/platform/internal/dialer/api"

// edge is a (current state, event) → next state mapping key.
type edge struct {
	from  api.State
	event api.Event
}

// transitions maps each valid (state, event) pair to the next state.
// Any pair not in this map is invalid and returns api.ErrInvalidTransition.
//
// The map is the single source of truth for valid edges; the FSM impl
// (Machine.applyEvent) consults it before issuing a Redis CAS. Force()
// bypasses this map entirely — it is the escape hatch by design.
//
// The diagram in package api/doc.go renders this map. Keep them in sync.
var transitions = map[edge]api.State{
	// Shift lifecycle
	{api.StateOffline, api.EventStartShift}: api.StateReady,
	{api.StateReady, api.EventEndShift}:     api.StateOffline,
	{api.StatePause, api.EventEndShift}:     api.StateOffline,
	{api.StateStatus, api.EventEndShift}:    api.StateOffline, // graceful end after wrap-up

	// Pause / resume. EventGoReady is treated as an alias for EventResume:
	// both are valid from pause. The operator-facing "Resume" button and
	// the supervisor-style "GoReady" call resolve to the same transition.
	{api.StateReady, api.EventGoPause}: api.StatePause,
	{api.StatePause, api.EventResume}:  api.StateReady,
	{api.StatePause, api.EventGoReady}: api.StateReady,

	// Dial → answer → call → status
	// Note: Ready→Dialing is the "dialing started" transition (operator
	// is now bound to a particular respondent attempt). Dialing→Call is
	// the "ANSWERED by callee" transition. Both are surfaced through the
	// single RecordCallStarted method; the impl distinguishes by the
	// operator's current state.
	{api.StateReady, api.EventCallStarted}:   api.StateDialing,
	{api.StateDialing, api.EventCallStarted}: api.StateCall,
	{api.StateDialing, api.EventCallEnded}:   api.StateReady, // hangup before answer (no-answer)
	{api.StateDialing, api.EventCallFailed}:  api.StateReady, // tech failure (busy/SIT/...)
	{api.StateCall, api.EventCallEnded}:      api.StateStatus,

	// Status submission
	{api.StateStatus, api.EventStatusSubmitted}: api.StateReady,

	// Verify (manager-mode listening to recordings)
	{api.StateStatus, api.EventGoVerify}:   api.StateVerify,
	{api.StateVerify, api.EventVerifyDone}: api.StateReady,

	// Force-offline is handled separately in Machine.Force(); not in this
	// map. EventForceOffline never reaches the transition lookup.
}

// IsTerminal reports whether s is a terminal state — one for which no
// further timed transition is expected. Used by metrics + the watchdog
// to short-circuit polling.
func IsTerminal(s api.State) bool {
	return s == api.StateOffline
}

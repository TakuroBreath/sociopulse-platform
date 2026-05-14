package fsm

import (
	"fmt"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// edge is a (current state, event) → next state mapping key.
type edge struct {
	from  api.State
	event api.Event
}

// OutcomeUnknown is the package-local alias for the api.StatusOutcome
// zero value ("") . Callers (machine.go, tests) use it to express
// "outcome not yet classified" without needing to repeat the canonical
// type prefix for every reference.
//
// OutcomeUnknown is NOT success-class (see api.StatusOutcome.IsSuccessClass),
// so a stale or unset carry-over CANNOT accidentally permit the
// (status, go_verify) → verify transition.
const OutcomeUnknown api.StatusOutcome = ""

// transitions maps each valid (state, event) pair to the next state.
// Any pair not in this map is invalid and returns api.ErrInvalidTransition.
//
// resolveTransition is the single decision point that wraps this table
// plus the outcome guard for (status, go_verify) — Force() bypasses both
// by design.
//
// The diagram in package api/doc.go renders this map. Keep them in sync.
//
// Spec coverage (CONTEXT.md authoritative):
//
//   - offline → ready → dialing → call → status → verify → ready
//   - pause is reachable from {ready, dialing, call, status, verify}
//     (the operator panic-pause feature — any non-offline / non-pause)
//   - verify is reachable ONLY from status, AND only when the carried
//     outcome is a success class (gated in resolveTransition, not here)
var transitions = map[edge]api.State{
	// Shift lifecycle
	{api.StateOffline, api.EventStartShift}: api.StateReady,
	{api.StateReady, api.EventEndShift}:     api.StateOffline,
	{api.StatePause, api.EventEndShift}:     api.StateOffline,
	{api.StateStatus, api.EventEndShift}:    api.StateOffline, // graceful end after wrap-up

	// Pause / resume. Pause is reachable from every non-offline /
	// non-pause state — the operator panic-pause is a user feature
	// (CONTEXT.md: "pause from any state"). Resume is the single
	// pause→ready edge; Machine.GoReady fires EventResume internally so
	// the operator-facing "Resume" button and the supervisor-style
	// "GoReady" call traverse the same edge.
	{api.StateReady, api.EventGoPause}:   api.StatePause,
	{api.StateDialing, api.EventGoPause}: api.StatePause,
	{api.StateCall, api.EventGoPause}:    api.StatePause,
	{api.StateStatus, api.EventGoPause}:  api.StatePause,
	{api.StateVerify, api.EventGoPause}:  api.StatePause,
	{api.StatePause, api.EventResume}:    api.StateReady,

	// Dial → answer → call → status
	// Note: Ready→Dialing is the "dialing started" transition (operator
	// is now bound to a particular respondent attempt). Dialing→Call is
	// the "ANSWERED by callee" transition. Both are surfaced through the
	// single RecordCallStarted method; the impl distinguishes by the
	// operator's current state.
	//
	// Dialing→Status covers the hangup-before-answer (EventCallEnded)
	// and tech-failure (EventCallFailed) paths. The operator MUST submit
	// a wrap-up disposition; status_rules then maps {busy, no_answer,
	// tech_failure, ...} into retry timings.
	//
	// RecordCallEnded carries an api.StatusOutcome that is stashed onto
	// the operator's `status` snapshot — this is what gates the eventual
	// (status, go_verify) → verify transition below.
	{api.StateReady, api.EventCallStarted}:   api.StateDialing,
	{api.StateDialing, api.EventCallStarted}: api.StateCall,
	{api.StateDialing, api.EventCallEnded}:   api.StateStatus,
	{api.StateDialing, api.EventCallFailed}:  api.StateStatus,
	{api.StateCall, api.EventCallEnded}:      api.StateStatus,

	// Status submission
	{api.StateStatus, api.EventStatusSubmitted}: api.StateReady,

	// Verify (supervisor-style listening to recordings). Reachable ONLY
	// from status with a success-class outcome (CONTEXT.md). The edge
	// is present here; resolveTransition additionally gates it on the
	// carried api.StatusOutcome before allowing the transition.
	{api.StateStatus, api.EventGoVerify}:   api.StateVerify,
	{api.StateVerify, api.EventVerifyDone}: api.StateReady,

	// Force-offline is handled separately in Machine.Force(); not in this
	// map. EventForceOffline never reaches the transition lookup.
}

// resolveTransition is the single decision point for an FSM step. It
// combines the static transitions table with the outcome guard required
// by CONTEXT.md for the (status, go_verify) → verify edge.
//
// outcome is consulted only for the (status, go_verify) edge; every
// other transition ignores it. The guard rejects non-success-class
// outcomes — including OutcomeUnknown — so a stale or unset outcome
// surfaces as ErrInvalidTransition rather than silently letting verify
// through.
//
// Returns:
//
//   - (target, nil) on a valid transition
//   - ("", ErrInvalidTransition wrapped) on an unrecognised edge OR on
//     a verify transition with a non-success outcome
//
// The error message is low-cardinality (from + event only, with the
// outcome label which is itself low-cardinality enum) — variable IDs
// belong in zap fields at the call site, not in this string.
func resolveTransition(from api.State, event api.Event, outcome api.StatusOutcome) (api.State, error) {
	next, ok := transitions[edge{from: from, event: event}]
	if !ok {
		return "", fmt.Errorf("%w: %s --%s-->", api.ErrInvalidTransition, from, event)
	}
	// Outcome guard for the verify entry edge — CONTEXT.md: "verify is
	// reachable only from success-class outcomes".
	if from == api.StateStatus && event == api.EventGoVerify && !outcome.IsSuccessClass() {
		return "", fmt.Errorf("%w: status --go_verify--> requires success-class outcome (got %q)",
			api.ErrInvalidTransition, outcome)
	}
	return next, nil
}

// IsTerminal reports whether s is a terminal state — one for which no
// further timed transition is expected. Used by metrics + the watchdog
// to short-circuit polling.
func IsTerminal(s api.State) bool {
	return s == api.StateOffline
}

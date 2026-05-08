package router

import (
	"github.com/google/uuid"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	telephonyapi "github.com/sociopulse/platform/internal/telephony/api"
)

// dialerEventType is the closed set of values the dialer's
// api.ChannelEvent.Type field accepts. Mirrors the documented projection
// described in the package doc — the dialer cares about three lifecycle
// stages (dialing, answered, hangup), not the seven raw FreeSWITCH
// channel events.
const (
	dialerEventDialing  = "dialing"
	dialerEventAnswered = "answered"
	dialerEventHangup   = "hangup"
)

// translateOriginate converts a dialer-side DialRequest into the
// telephony bridge's OriginateCommand wire shape.
//
// CommandID is freshly minted per call (uuid.New). The bridge's
// SETNX-on-CommandID idempotency layer treats this token as the unit
// of replay protection — see the package doc for the contract: a
// dialer-level retry MUST mint a fresh CommandID, not reuse the
// previous one, otherwise the bridge will treat the second publish as
// an idempotent replay of the first failed command and refuse to
// originate. This package mints fresh inside Dial() so callers can
// safely retry the dialer.api.Router.Dial call as many times as they
// like; each lands as a distinct bridge command.
//
// Field-by-field mapping:
//
//   - CallID         passes through unchanged (cross-boundary identity).
//   - TenantID       passes through unchanged.
//   - OperatorExt    passes through unchanged (SIP user).
//   - Phone          → Number (telephony names the field Number).
//   - FsNode         → FSNode (the telephony struct uses uppercase FSNode).
//
// Fields the dialer does NOT populate today — TrunkID, PromptURL,
// RecordingPath, CallerID, DialingTimeout — are left at their zero
// values. The bridge / Router has its own defaults (Plan 09) and the
// dialer's call sites do not yet thread these through. When they do
// (Plan 10 Task 8 retry orchestrator timeouts; Plan 12 prompts) the
// translation grows here, not at call sites.
//
// OperatorID and ProjectID from DialRequest are retained on the dialer
// side for lifecycle-event correlation; the bridge does not need them
// for the originate itself, so they are NOT passed in the
// OriginateCommand. The dialer's CallLifecycleEvent (events.go) is the
// canonical place where OperatorID surfaces to downstream consumers.
func translateOriginate(req dialerapi.DialRequest) telephonyapi.OriginateCommand {
	return telephonyapi.OriginateCommand{
		CommandID:   uuid.New(),
		TenantID:    req.TenantID,
		CallID:      req.CallID,
		OperatorExt: req.OperatorExt,
		Number:      req.Phone,
		FSNode:      req.FsNode,
	}
}

// translateHangup converts a (callID, reason) pair into the telephony
// bridge's HangupCommand wire shape. Like translateOriginate, the
// CommandID is freshly minted per call so the bridge's idempotency
// layer treats each Hangup publish as a distinct command.
//
// The reason string is forwarded to telephony.HangupCommand.Cause
// verbatim. Acceptable values match FreeSWITCH hangup-cause names
// (e.g. NORMAL_CLEARING, USER_BUSY, NO_ANSWER); the bridge passes
// these to mod_dptools without further validation. An empty reason is
// allowed — the bridge defaults to NORMAL_CLEARING in that case.
func translateHangup(callID uuid.UUID, reason string) telephonyapi.HangupCommand {
	return telephonyapi.HangupCommand{
		CommandID: uuid.New(),
		CallID:    callID,
		Cause:     reason,
	}
}

// translateChannelEvent projects a telephony.api.ChannelEvent into the
// dialer's reduced lifecycle view. Returns ok=false to signal a drop —
// the caller increments dialer_router_events_dropped_total and skips
// the dialer handler entirely.
//
// Mapping table (kept identical to the package doc and to the tests):
//
//	telephony.EventDialing      → dialer "dialing"   (ok=true)
//	telephony.EventAnswer       → dialer "answered"  (ok=true)
//	telephony.EventBridge       → dialer "answered"  (ok=true) — already-answered confirmation
//	telephony.EventUnbridge     → drop (intermediate state, no dialer projection)
//	telephony.EventHangup       → dialer "hangup"    (ok=true)
//	telephony.EventDTMF         → drop (mediation layer's concern)
//	telephony.EventRecordStop   → drop (recording's concern)
//
// The third return value reports whether the input event type is one
// the translator recognises at all. An unknown type (a future telephony
// enum addition this package has not yet been taught about) yields
// known=false; the caller increments
// dialer_router_events_translation_errors_total in addition to the
// dropped counter, surfacing the gap in the metric stream.
//
// Pass-through fields:
//
//   - CallID    — the cross-boundary identity token.
//   - HangupCause       → Cause     (only meaningful on hangup).
//   - DurationMS        → Duration  (only meaningful on hangup).
//   - FSNode            → FsNode    (passed through verbatim).
//
// The dialer's ChannelEvent intentionally drops EventID, TenantID,
// SIPResponse, Timestamp, and Headers; those belong to telephony's
// audit / analytics surface, not the dialer's lifecycle view. A
// downstream consumer that needs them subscribes to the raw telephony
// subjects directly.
func translateChannelEvent(evt telephonyapi.ChannelEvent) (dialerapi.ChannelEvent, bool, bool) {
	dialerType, ok, known := mapChannelEventType(evt.Type)
	if !ok {
		return dialerapi.ChannelEvent{}, false, known
	}
	out := dialerapi.ChannelEvent{
		CallID: evt.CallID,
		Type:   dialerType,
		FsNode: evt.FSNode,
	}
	if evt.Type == telephonyapi.EventHangup {
		out.Cause = evt.HangupCause
		// DurationMS on telephony.ChannelEvent is int64; the dialer's
		// Duration field is `int` (ms). Real durations fit comfortably
		// in 32-bit signed ms range (~24 days), so the conversion is
		// safe in practice. We clamp negatives to 0 because the dialer
		// treats Duration as a non-negative ms count and a future
		// upstream bug must not surface as a stale negative number.
		// max(_, 0) clamps a stale negative value rather than letting
		// it surface as a -1 ms duration to the FSM/audit/billing.
		out.Duration = int(max(evt.DurationMS, 0))
	}
	return out, true, true
}

// mapChannelEventType is the pure-function discriminator behind
// translateChannelEvent. Split out so the table is easy to audit and
// to test in isolation. Returns:
//
//   - dialerType: the projected dialer.api.ChannelEvent.Type string
//     when ok=true; empty string when ok=false.
//   - ok:         true when this event SHOULD reach the dialer's
//     handler (after pass-through field projection); false to signal
//     a drop (Unbridge / DTMF / RecordStop today).
//   - known:      true when the input is a recognised
//     telephony.ChannelEventType constant; false when it's something
//     this package has not been taught about. ok=false implies a drop;
//     known=false additionally flags a translation error to the metric.
func mapChannelEventType(t telephonyapi.ChannelEventType) (dialerType string, ok bool, known bool) {
	switch t {
	case telephonyapi.EventDialing:
		return dialerEventDialing, true, true
	case telephonyapi.EventAnswer, telephonyapi.EventBridge:
		return dialerEventAnswered, true, true
	case telephonyapi.EventHangup:
		return dialerEventHangup, true, true
	case telephonyapi.EventUnbridge,
		telephonyapi.EventDTMF,
		telephonyapi.EventRecordStop:
		return "", false, true
	default:
		return "", false, false
	}
}

package esl

import (
	"maps"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/internal/telephony/api"
)

// MapEvent translates a raw FreeSWITCH ESL Event into the bridge's
// public api.ChannelEvent DTO. Returns ok=false when the event name is
// not one we route on (HEARTBEAT, BACKGROUND_JOB, sofia::register, etc.
// — those don't carry per-call state).
//
// MapEvent is intentionally narrow: it fills only fields that an event
// source can populate. TenantID and EventID are assigned by higher
// layers (Task 4 pool fanout / Task 3 NATS bridge) where the per-call
// metadata table is consulted. CallID is a parse of the FS Unique-ID
// header — when that header is not a valid UUID the function still
// returns the event but with a zero CallID, so the caller can decide
// whether to ignore or salvage.
//
// References doc gotcha #7 specifies the events Plan 09 subscribes to:
// CHANNEL_CREATE, CHANNEL_ANSWER, CHANNEL_HANGUP_COMPLETE,
// CHANNEL_BRIDGE, CHANNEL_UNBRIDGE, DTMF, RECORD_STOP. Anything outside
// that list returns ok=false; the caller is expected to log+drop.
func MapEvent(ev Event) (api.ChannelEvent, bool) {
	t, ok := mapEventType(ev.Name)
	if !ok {
		return api.ChannelEvent{}, false
	}

	// Clone the per-event header bag so consumers cannot mutate parser
	// internals — the Event value is intended to be immutable from the
	// caller's perspective, and aliasing the underlying map would let a
	// downstream component corrupt subsequent MapEvent invocations on
	// the same Event.
	out := api.ChannelEvent{
		Type:      t,
		Timestamp: parseEventTimestamp(ev),
		Headers:   maps.Clone(ev.headers),
	}

	if ev.UUID != "" {
		if id, err := uuid.Parse(ev.UUID); err == nil {
			out.CallID = id
		}
	}

	if t == api.EventHangup {
		out.HangupCause = ev.Header("Hangup-Cause")
		if sip := ev.Header("variable_sip_term_status"); sip != "" {
			if n, err := strconv.Atoi(sip); err == nil {
				out.SIPResponse = n
			}
		}
		if dur := ev.Header("variable_billsec"); dur != "" {
			if n, err := strconv.ParseInt(dur, 10, 64); err == nil {
				out.DurationMS = n * 1000
			}
		}
	}

	return out, true
}

// mapEventType is the FS event-name → api.ChannelEventType lookup.
// Plan 09 subscribes to a small fixed set; anything outside that
// returns ok=false. Centralising here keeps the policy in one place.
func mapEventType(fsName string) (api.ChannelEventType, bool) {
	switch fsName {
	case "CHANNEL_CREATE":
		return api.EventDialing, true
	case "CHANNEL_ANSWER":
		return api.EventAnswer, true
	case "CHANNEL_BRIDGE":
		return api.EventBridge, true
	case "CHANNEL_UNBRIDGE":
		return api.EventUnbridge, true
	case "CHANNEL_HANGUP_COMPLETE":
		return api.EventHangup, true
	case "DTMF":
		return api.EventDTMF, true
	case "RECORD_STOP":
		return api.EventRecordStop, true
	default:
		return "", false
	}
}

// parseEventTimestamp pulls the FS event timestamp from one of the
// per-event headers (preferring Event-Date-Timestamp, FS's microsecond
// epoch). Falls back to wall clock when the header is missing or
// unparseable — better than zeroing out for downstream consumers that
// rely on a non-zero Timestamp for ordering.
func parseEventTimestamp(ev Event) time.Time {
	if v := ev.Header("Event-Date-Timestamp"); v != "" {
		if usec, err := strconv.ParseInt(v, 10, 64); err == nil && usec > 0 {
			// FS exposes timestamps as microseconds since UNIX epoch.
			return time.UnixMicro(usec)
		}
	}
	return time.Now()
}

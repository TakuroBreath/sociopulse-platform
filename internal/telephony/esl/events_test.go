package esl

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/telephony/api"
)

// makeEvent builds an Event directly from a header bag. Tests use this
// instead of round-tripping through parseFrame because the mapping is
// the unit under test, not the parser.
func makeEvent(name, callUUID string, extra map[string]string) Event {
	headers := map[string]string{}
	if name != "" {
		headers["event-name"] = name
	}
	if callUUID != "" {
		headers["unique-id"] = callUUID
	}
	for k, v := range extra {
		headers[k] = v
	}
	return Event{Name: name, UUID: callUUID, headers: headers}
}

func TestMapEvent_ChannelCreateMapsToDialing(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	ev, ok := MapEvent(makeEvent("CHANNEL_CREATE", id.String(), nil))
	require.True(t, ok)
	require.Equal(t, api.EventDialing, ev.Type)
	require.Equal(t, id, ev.CallID)
	require.False(t, ev.Timestamp.IsZero())
}

func TestMapEvent_ChannelAnswerMapsToAnswer(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	ev, ok := MapEvent(makeEvent("CHANNEL_ANSWER", id.String(), nil))
	require.True(t, ok)
	require.Equal(t, api.EventAnswer, ev.Type)
	require.Equal(t, id, ev.CallID)
}

func TestMapEvent_ChannelBridgeMapsToBridge(t *testing.T) {
	t.Parallel()
	ev, ok := MapEvent(makeEvent("CHANNEL_BRIDGE", uuid.New().String(), nil))
	require.True(t, ok)
	require.Equal(t, api.EventBridge, ev.Type)
}

func TestMapEvent_ChannelUnbridgeMapsToUnbridge(t *testing.T) {
	t.Parallel()
	ev, ok := MapEvent(makeEvent("CHANNEL_UNBRIDGE", uuid.New().String(), nil))
	require.True(t, ok)
	require.Equal(t, api.EventUnbridge, ev.Type)
}

func TestMapEvent_HangupCompleteCarriesCauseAndDuration(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	ev, ok := MapEvent(makeEvent("CHANNEL_HANGUP_COMPLETE", id.String(), map[string]string{
		"hangup-cause":             "USER_BUSY",
		"variable_sip_term_status": "486",
		"variable_billsec":         "12",
	}))
	require.True(t, ok)
	require.Equal(t, api.EventHangup, ev.Type)
	require.Equal(t, "USER_BUSY", ev.HangupCause)
	require.Equal(t, 486, ev.SIPResponse)
	require.Equal(t, int64(12000), ev.DurationMS)
}

func TestMapEvent_HangupCompleteToleratesMissingNumericFields(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	ev, ok := MapEvent(makeEvent("CHANNEL_HANGUP_COMPLETE", id.String(), map[string]string{
		"hangup-cause":             "NORMAL_CLEARING",
		"variable_sip_term_status": "not-a-number",
		"variable_billsec":         "",
	}))
	require.True(t, ok)
	require.Equal(t, "NORMAL_CLEARING", ev.HangupCause)
	require.Equal(t, 0, ev.SIPResponse)
	require.Equal(t, int64(0), ev.DurationMS)
}

func TestMapEvent_DTMFMapsToDTMF(t *testing.T) {
	t.Parallel()
	ev, ok := MapEvent(makeEvent("DTMF", uuid.New().String(), nil))
	require.True(t, ok)
	require.Equal(t, api.EventDTMF, ev.Type)
}

func TestMapEvent_RecordStopMapsToRecordStop(t *testing.T) {
	t.Parallel()
	ev, ok := MapEvent(makeEvent("RECORD_STOP", uuid.New().String(), nil))
	require.True(t, ok)
	require.Equal(t, api.EventRecordStop, ev.Type)
}

func TestMapEvent_UnknownEventReturnsFalse(t *testing.T) {
	t.Parallel()
	cases := []string{"HEARTBEAT", "BACKGROUND_JOB", "sofia::register", "", "UNRELATED"}
	for _, name := range cases {
		_, ok := MapEvent(makeEvent(name, uuid.New().String(), nil))
		require.False(t, ok, "expected ok=false for %q", name)
	}
}

func TestMapEvent_InvalidUUIDLeavesCallIDZero(t *testing.T) {
	t.Parallel()
	ev, ok := MapEvent(makeEvent("CHANNEL_CREATE", "not-a-uuid", nil))
	require.True(t, ok)
	require.Equal(t, uuid.Nil, ev.CallID)
}

func TestMapEvent_TimestampParsedFromFSHeader(t *testing.T) {
	t.Parallel()
	want := time.Now().UTC().Truncate(time.Microsecond)
	ev, ok := MapEvent(makeEvent("CHANNEL_CREATE", uuid.New().String(), map[string]string{
		"event-date-timestamp": strconv.FormatInt(want.UnixMicro(), 10),
	}))
	require.True(t, ok)
	require.Equal(t, want.UnixMicro(), ev.Timestamp.UnixMicro())
}

func TestMapEvent_TimestampFallsBackToNowOnUnparseableHeader(t *testing.T) {
	t.Parallel()
	before := time.Now().Add(-1 * time.Second)
	ev, ok := MapEvent(makeEvent("CHANNEL_CREATE", uuid.New().String(), map[string]string{
		"event-date-timestamp": "garbage",
	}))
	require.True(t, ok)
	require.True(t, ev.Timestamp.After(before),
		"timestamp should fall back to now() on unparseable input")
}

func TestMapEvent_HeadersPropagated(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	ev, ok := MapEvent(makeEvent("CHANNEL_CREATE", id.String(), map[string]string{
		"caller-caller-id-number": "+79991234567",
	}))
	require.True(t, ok)
	require.Equal(t, "+79991234567", ev.Headers["caller-caller-id-number"])
}

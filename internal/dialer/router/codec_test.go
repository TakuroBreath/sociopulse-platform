package router

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	telephonyapi "github.com/sociopulse/platform/internal/telephony/api"
)

// TestTranslateOriginate_FieldMapping — every DialRequest field that
// the OriginateCommand has a slot for is forwarded verbatim, and the
// CommandID is non-zero (uuid.New) so the bridge's idempotency key is
// always present.
func TestTranslateOriginate_FieldMapping(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	callID := uuid.New()
	operatorID := uuid.New()
	projectID := uuid.New()

	req := dialerapi.DialRequest{
		CallID:       callID,
		TenantID:     tenantID,
		OperatorID:   operatorID,
		RespondentID: uuid.New(),
		ProjectID:    projectID,
		OperatorExt:  "lst_42",
		Phone:        "+79991234567",
		FsNode:       "fs1.example.com:8021",
	}

	cmd := translateOriginate(req)

	require.Equal(t, callID, cmd.CallID)
	require.Equal(t, tenantID, cmd.TenantID)
	require.Equal(t, "lst_42", cmd.OperatorExt)
	require.Equal(t, "+79991234567", cmd.Number)
	require.Equal(t, "fs1.example.com:8021", cmd.FSNode)
	require.NotEqual(t, uuid.Nil, cmd.CommandID, "CommandID must be freshly minted, not nil")
}

// TestTranslateOriginate_FreshCommandIDs — two calls with the same
// DialRequest produce DIFFERENT CommandIDs. This is the contract that
// lets a dialer-level retry land as a NEW bridge command (the bridge's
// SETNX-on-CommandID idempotency would otherwise treat the second
// publish as a replay of the first).
func TestTranslateOriginate_FreshCommandIDs(t *testing.T) {
	t.Parallel()

	req := dialerapi.DialRequest{
		CallID:      uuid.New(),
		TenantID:    uuid.New(),
		OperatorExt: "lst_42",
		Phone:       "+79991234567",
		FsNode:      "fs1.example.com:8021",
	}

	a := translateOriginate(req)
	b := translateOriginate(req)
	require.NotEqual(t, a.CommandID, b.CommandID,
		"each translateOriginate must mint a fresh CommandID for bridge idempotency")
}

// TestTranslateHangup_FieldMapping — callID + reason flow through to
// HangupCommand; CommandID is freshly minted.
func TestTranslateHangup_FieldMapping(t *testing.T) {
	t.Parallel()

	callID := uuid.New()
	cmd := translateHangup(callID, "USER_BUSY")

	require.Equal(t, callID, cmd.CallID)
	require.Equal(t, "USER_BUSY", cmd.Cause)
	require.NotEqual(t, uuid.Nil, cmd.CommandID, "CommandID must be freshly minted")
}

// TestTranslateHangup_EmptyReasonPassThrough — empty reason is allowed
// (the bridge defaults to NORMAL_CLEARING). The translator does NOT
// substitute a default; it forwards verbatim.
func TestTranslateHangup_EmptyReasonPassThrough(t *testing.T) {
	t.Parallel()
	cmd := translateHangup(uuid.New(), "")
	require.Empty(t, cmd.Cause)
}

// TestTranslateHangup_FreshCommandIDs — symmetry with the originate
// case: each call mints a fresh CommandID.
func TestTranslateHangup_FreshCommandIDs(t *testing.T) {
	t.Parallel()
	callID := uuid.New()
	a := translateHangup(callID, "NORMAL_CLEARING")
	b := translateHangup(callID, "NORMAL_CLEARING")
	require.NotEqual(t, a.CommandID, b.CommandID)
}

// TestMapChannelEventType — table-driven coverage of every
// telephony.ChannelEventType plus an "unknown" probe. The triplet of
// returns (dialerType, ok, known) is asserted on each row.
func TestMapChannelEventType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		in             telephonyapi.ChannelEventType
		wantDialerType string
		wantOK         bool
		wantKnown      bool
	}{
		{
			name:           "Dialing → dialing",
			in:             telephonyapi.EventDialing,
			wantDialerType: dialerEventDialing,
			wantOK:         true,
			wantKnown:      true,
		},
		{
			name:           "Answer → answered",
			in:             telephonyapi.EventAnswer,
			wantDialerType: dialerEventAnswered,
			wantOK:         true,
			wantKnown:      true,
		},
		{
			name:           "Bridge folds into answered",
			in:             telephonyapi.EventBridge,
			wantDialerType: dialerEventAnswered,
			wantOK:         true,
			wantKnown:      true,
		},
		{
			name:           "Hangup → hangup",
			in:             telephonyapi.EventHangup,
			wantDialerType: dialerEventHangup,
			wantOK:         true,
			wantKnown:      true,
		},
		{
			name:           "Unbridge dropped (intermediate)",
			in:             telephonyapi.EventUnbridge,
			wantDialerType: "",
			wantOK:         false,
			wantKnown:      true,
		},
		{
			name:           "DTMF dropped (mediation)",
			in:             telephonyapi.EventDTMF,
			wantDialerType: "",
			wantOK:         false,
			wantKnown:      true,
		},
		{
			name:           "RecordStop dropped (recording)",
			in:             telephonyapi.EventRecordStop,
			wantDialerType: "",
			wantOK:         false,
			wantKnown:      true,
		},
		{
			name:           "Unknown event type drops + flags translation error",
			in:             telephonyapi.ChannelEventType("future_event"),
			wantDialerType: "",
			wantOK:         false,
			wantKnown:      false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			gotDialer, gotOK, gotKnown := mapChannelEventType(c.in)
			require.Equal(t, c.wantDialerType, gotDialer)
			require.Equal(t, c.wantOK, gotOK)
			require.Equal(t, c.wantKnown, gotKnown)
		})
	}
}

// TestTranslateChannelEvent_PassThroughOnAnswered — pass-through fields
// (CallID, FsNode) are forwarded; Cause/Duration are NOT (only
// meaningful on hangup).
func TestTranslateChannelEvent_PassThroughOnAnswered(t *testing.T) {
	t.Parallel()
	callID := uuid.New()
	tenantID := uuid.New()

	in := telephonyapi.ChannelEvent{
		EventID:     uuid.New(),
		TenantID:    tenantID,
		CallID:      callID,
		FSNode:      "fs2.example.com:8021",
		Type:        telephonyapi.EventAnswer,
		HangupCause: "should-be-dropped",
		DurationMS:  9999,
		Timestamp:   time.Now(),
	}

	out, ok, known := translateChannelEvent(in)
	require.True(t, ok)
	require.True(t, known)
	require.Equal(t, callID, out.CallID)
	require.Equal(t, dialerEventAnswered, out.Type)
	require.Equal(t, "fs2.example.com:8021", out.FsNode)
	require.Empty(t, out.Cause, "Cause is hangup-only — must be empty on answered")
	require.Equal(t, 0, out.Duration, "Duration is hangup-only — must be 0 on answered")
}

// TestTranslateChannelEvent_PassThroughOnHangup — on hangup the Cause
// and Duration ARE forwarded (they are the hangup-shape fields).
func TestTranslateChannelEvent_PassThroughOnHangup(t *testing.T) {
	t.Parallel()
	callID := uuid.New()

	in := telephonyapi.ChannelEvent{
		CallID:      callID,
		FSNode:      "fs3.example.com:8021",
		Type:        telephonyapi.EventHangup,
		HangupCause: "USER_BUSY",
		DurationMS:  4250,
	}

	out, ok, known := translateChannelEvent(in)
	require.True(t, ok)
	require.True(t, known)
	require.Equal(t, callID, out.CallID)
	require.Equal(t, dialerEventHangup, out.Type)
	require.Equal(t, "fs3.example.com:8021", out.FsNode)
	require.Equal(t, "USER_BUSY", out.Cause)
	require.Equal(t, 4250, out.Duration)
}

// TestTranslateChannelEvent_NegativeDurationClampedToZero — defensive
// guard: a negative DurationMS from upstream (bug or wire corruption)
// must not surface to the dialer as a negative ms count.
func TestTranslateChannelEvent_NegativeDurationClampedToZero(t *testing.T) {
	t.Parallel()
	in := telephonyapi.ChannelEvent{
		CallID:      uuid.New(),
		Type:        telephonyapi.EventHangup,
		HangupCause: "NORMAL_CLEARING",
		DurationMS:  -1,
	}
	out, ok, known := translateChannelEvent(in)
	require.True(t, ok)
	require.True(t, known)
	require.Equal(t, 0, out.Duration)
}

// TestTranslateChannelEvent_DroppedTypes — dropped types yield ok=false
// and an empty event. Coverage is per-type so a future regression on
// any single drop case is pinpointed.
func TestTranslateChannelEvent_DroppedTypes(t *testing.T) {
	t.Parallel()
	dropped := []telephonyapi.ChannelEventType{
		telephonyapi.EventUnbridge,
		telephonyapi.EventDTMF,
		telephonyapi.EventRecordStop,
	}
	for _, et := range dropped {
		t.Run(string(et), func(t *testing.T) {
			t.Parallel()
			in := telephonyapi.ChannelEvent{
				CallID: uuid.New(),
				Type:   et,
			}
			out, ok, known := translateChannelEvent(in)
			require.False(t, ok)
			require.True(t, known, "drop is intentional, not unknown")
			require.Equal(t, dialerapi.ChannelEvent{}, out, "dropped event yields zero value")
		})
	}
}

// TestTranslateChannelEvent_UnknownType — an unrecognised future enum
// addition drops AND signals known=false so the translator's caller
// can tick the EventsTranslationErrors metric.
func TestTranslateChannelEvent_UnknownType(t *testing.T) {
	t.Parallel()
	in := telephonyapi.ChannelEvent{
		CallID: uuid.New(),
		Type:   telephonyapi.ChannelEventType("not-a-real-event"),
	}
	out, ok, known := translateChannelEvent(in)
	require.False(t, ok)
	require.False(t, known)
	require.Equal(t, dialerapi.ChannelEvent{}, out)
}

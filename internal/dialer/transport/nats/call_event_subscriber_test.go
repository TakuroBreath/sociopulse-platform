package nats_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	dialernats "github.com/sociopulse/platform/internal/dialer/transport/nats"
	telapi "github.com/sociopulse/platform/internal/telephony/api"
)

// fakeFSM records every OperatorFSM call. Only the three methods used by
// the call-event subscriber are needed in detail; the rest satisfy the
// interface as no-ops so the subscriber compiles against the same
// surface cmd/api uses in production.
type fakeFSM struct {
	mu sync.Mutex

	startedCalls []dialerapi.CallStartedRequest
	endedCalls   []dialerapi.CallEndedRequest

	startedErr error
	endedErr   error

	startedCount atomic.Int32
	endedCount   atomic.Int32
}

func (f *fakeFSM) RecordCallStarted(_ context.Context, req dialerapi.CallStartedRequest) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startedCalls = append(f.startedCalls, req)
	f.startedCount.Add(1)
	if f.startedErr != nil {
		return dialerapi.Snapshot{}, f.startedErr
	}
	return dialerapi.Snapshot{
		TenantID:      req.TenantID,
		OperatorID:    req.OperatorID,
		State:         dialerapi.StateCall,
		CurrentCallID: &req.CallID,
	}, nil
}

func (f *fakeFSM) RecordCallEnded(_ context.Context, req dialerapi.CallEndedRequest) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endedCalls = append(f.endedCalls, req)
	f.endedCount.Add(1)
	if f.endedErr != nil {
		return dialerapi.Snapshot{}, f.endedErr
	}
	return dialerapi.Snapshot{
		TenantID:   req.TenantID,
		OperatorID: req.OperatorID,
		State:      dialerapi.StateStatus,
		Outcome:    req.Outcome,
	}, nil
}

// unused OperatorFSM methods. Implemented so *fakeFSM satisfies
// dialerapi.OperatorFSM and the subscriber's compile-time interface
// check catches signature drift.
func (f *fakeFSM) StartShift(context.Context, dialerapi.StartShiftRequest) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

func (f *fakeFSM) EndShift(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fakeFSM) GoReady(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

func (f *fakeFSM) GoPause(context.Context, dialerapi.GoPauseRequest) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fakeFSM) Resume(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

func (f *fakeFSM) SubmitStatus(context.Context, dialerapi.SubmitStatusRequest) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fakeFSM) GoVerify(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fakeFSM) VerifyDone(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fakeFSM) GetState(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

func (f *fakeFSM) Force(context.Context, uuid.UUID, uuid.UUID, dialerapi.State, dialerapi.ForceReason) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

// Compile-time check: *fakeFSM satisfies dialerapi.OperatorFSM.
var _ dialerapi.OperatorFSM = (*fakeFSM)(nil)

// fakeCallLookup resolves (tenant_id, call_id) → operator_id in-memory.
// Mirrors the production *pg.Pool path the subscriber depends on; tests
// pre-seed the desired binding before publishing the event.
type fakeCallLookup struct {
	mu       sync.Mutex
	bindings map[string]uuid.UUID // key = tenantID + ":" + callID
	err      error
}

func newFakeCallLookup() *fakeCallLookup {
	return &fakeCallLookup{bindings: map[string]uuid.UUID{}}
}

func (l *fakeCallLookup) bind(tenantID, callID, operatorID uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bindings[tenantID.String()+":"+callID.String()] = operatorID
}

func (l *fakeCallLookup) LookupOperator(_ context.Context, tenantID, callID uuid.UUID) (uuid.UUID, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return uuid.Nil, l.err
	}
	if op, ok := l.bindings[tenantID.String()+":"+callID.String()]; ok {
		return op, nil
	}
	return uuid.Nil, dialernats.ErrOperatorNotFound
}

// Compile-time check: *fakeCallLookup satisfies dialernats.CallOperatorLookup.
var _ dialernats.CallOperatorLookup = (*fakeCallLookup)(nil)

// fakeBus mirrors analytics/service ingest_test.go's fakeBus: a thin
// in-memory eventbus.Subscriber that records handlers per subject and
// lets tests synchronously dispatch a payload to the registered handler.
//
// Real NATS subscriptions deliver to wildcard handlers based on subject
// match. The fake stores the (subject_pattern, handler) pair; Publish
// looks up by exact pattern OR wildcard prefix.
type fakeBus struct {
	mu       sync.Mutex
	handlers []handlerEntry
}

type handlerEntry struct {
	subject string
	queue   string
	handler func(string, []byte) error
}

func newFakeBus() *fakeBus {
	return &fakeBus{}
}

func (b *fakeBus) Subscribe(_ context.Context, subj, queue string, h func(string, []byte) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers = append(b.handlers, handlerEntry{subject: subj, queue: queue, handler: h})
	return nil
}

func (b *fakeBus) handlerCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.handlers)
}

// Publish synchronously invokes every handler whose subject pattern
// matches concrete. Matching mirrors NATS wildcard semantics for the
// patterns we use: literal segments must match, `*` matches one token,
// `>` matches one-or-more trailing tokens.
func (b *fakeBus) Publish(t *testing.T, concrete string, payload []byte) error {
	t.Helper()
	b.mu.Lock()
	entries := make([]handlerEntry, len(b.handlers))
	copy(entries, b.handlers)
	b.mu.Unlock()
	matched := 0
	var firstErr error
	for _, e := range entries {
		if !subjectMatches(e.subject, concrete) {
			continue
		}
		matched++
		if err := e.handler(concrete, payload); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	require.Positive(t, matched, "fakeBus: no handler matched subject %q", concrete)
	return firstErr
}

// subjectMatches replicates NATS-style subject pattern matching for
// the literals used in this test file. Intentionally minimal — handles
// `*` (single token) and `>` (trailing match) only.
func subjectMatches(pattern, concrete string) bool {
	pp := splitDot(pattern)
	cc := splitDot(concrete)
	for i, p := range pp {
		if p == ">" {
			return true
		}
		if i >= len(cc) {
			return false
		}
		if p == "*" {
			continue
		}
		if p != cc[i] {
			return false
		}
	}
	return len(pp) == len(cc)
}

func splitDot(s string) []string {
	var out []string
	start := 0
	for i := range len(s) {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// newAnsweredEvent constructs a well-formed Type=answer ChannelEvent
// the subscriber will route through RecordCallStarted.
func newAnsweredEvent(tenantID, callID uuid.UUID) telapi.ChannelEvent {
	return telapi.ChannelEvent{
		EventID:    uuid.New(),
		TenantID:   tenantID,
		CallID:     callID,
		FSNode:     "fs-01",
		Type:       telapi.EventAnswer,
		DurationMS: 0,
		Timestamp:  time.Now().UTC(),
	}
}

// newHangupEvent constructs a well-formed Type=hangup ChannelEvent with
// the supplied cause.
func newHangupEvent(tenantID, callID uuid.UUID, cause string) telapi.ChannelEvent {
	return telapi.ChannelEvent{
		EventID:     uuid.New(),
		TenantID:    tenantID,
		CallID:      callID,
		FSNode:      "fs-01",
		Type:        telapi.EventHangup,
		HangupCause: cause,
		DurationMS:  12345,
		Timestamp:   time.Now().UTC(),
	}
}

func marshalEvent(t *testing.T, ev telapi.ChannelEvent) []byte {
	t.Helper()
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	return raw
}

// TestCallEventSubscriber_AnsweredEvent_FiresRecordCallStarted is the
// happy-path test for the answer → call FSM transition.
func TestCallEventSubscriber_AnsweredEvent_FiresRecordCallStarted(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	fsm := &fakeFSM{}
	lookup := newFakeCallLookup()
	sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))

	require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))
	require.Equal(t, 1, bus.handlerCount())

	tenantID := uuid.New()
	operatorID := uuid.New()
	callID := uuid.New()
	lookup.bind(tenantID, callID, operatorID)

	ev := newAnsweredEvent(tenantID, callID)
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventAnswer))
	require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)))

	require.Equal(t, int32(1), fsm.startedCount.Load())
	require.Equal(t, int32(0), fsm.endedCount.Load())

	fsm.mu.Lock()
	got := fsm.startedCalls[0]
	fsm.mu.Unlock()
	assert.Equal(t, tenantID, got.TenantID)
	assert.Equal(t, operatorID, got.OperatorID)
	assert.Equal(t, callID, got.CallID)
}

// TestCallEventSubscriber_HangupEvent_NormalClearing_FiresRecordCallEnded_WithOutcomeSuccess
// asserts that NORMAL_CLEARING → OutcomeSuccess and routes to
// RecordCallEnded with the resolved operator_id.
func TestCallEventSubscriber_HangupEvent_NormalClearing_FiresRecordCallEnded_WithOutcomeSuccess(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	fsm := &fakeFSM{}
	lookup := newFakeCallLookup()
	sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

	tenantID := uuid.New()
	operatorID := uuid.New()
	callID := uuid.New()
	lookup.bind(tenantID, callID, operatorID)

	ev := newHangupEvent(tenantID, callID, "NORMAL_CLEARING")
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventHangup))
	require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)))

	require.Equal(t, int32(1), fsm.endedCount.Load())

	fsm.mu.Lock()
	got := fsm.endedCalls[0]
	fsm.mu.Unlock()
	assert.Equal(t, tenantID, got.TenantID)
	assert.Equal(t, operatorID, got.OperatorID)
	assert.Equal(t, callID, got.CallID)
	assert.Equal(t, "NORMAL_CLEARING", got.Cause)
	assert.Equal(t, dialerapi.OutcomeSuccess, got.Outcome)
}

// TestCallEventSubscriber_HangupEvent_OutcomeMapping is the table-driven
// matrix of hangup_cause → StatusOutcome. Each row is a published event;
// the assertion checks the RecordCallEnded request outcome.
func TestCallEventSubscriber_HangupEvent_OutcomeMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		hangupCause string
		want        dialerapi.StatusOutcome
	}{
		{name: "NormalClearing_Success", hangupCause: "NORMAL_CLEARING", want: dialerapi.OutcomeSuccess},
		{name: "UserBusy_Busy", hangupCause: "USER_BUSY", want: dialerapi.OutcomeBusy},
		{name: "NoAnswer_NoAnswer", hangupCause: "NO_USER_RESPONSE", want: dialerapi.OutcomeNoAnswer},
		{name: "NoUserResponseAnswerTimeout_NoAnswer", hangupCause: "NO_ANSWER", want: dialerapi.OutcomeNoAnswer},
		{name: "CallRejected_TechFailure", hangupCause: "CALL_REJECTED", want: dialerapi.OutcomeTechFailure},
		{name: "NormalTempFailure_TechFailure", hangupCause: "NORMAL_TEMPORARY_FAILURE", want: dialerapi.OutcomeTechFailure},
		{name: "DestinationOutOfOrder_TechFailure", hangupCause: "DESTINATION_OUT_OF_ORDER", want: dialerapi.OutcomeTechFailure},
		{name: "Unknown_TechFailureFailSafe", hangupCause: "SOME_UNEXPECTED_CAUSE", want: dialerapi.OutcomeTechFailure},
		{name: "EmptyCause_TechFailureFailSafe", hangupCause: "", want: dialerapi.OutcomeTechFailure},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bus := newFakeBus()
			fsm := &fakeFSM{}
			lookup := newFakeCallLookup()
			sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
			require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

			tenantID := uuid.New()
			operatorID := uuid.New()
			callID := uuid.New()
			lookup.bind(tenantID, callID, operatorID)

			ev := newHangupEvent(tenantID, callID, tc.hangupCause)
			subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventHangup))
			require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)))

			require.Equal(t, int32(1), fsm.endedCount.Load())
			fsm.mu.Lock()
			got := fsm.endedCalls[0]
			fsm.mu.Unlock()
			assert.Equal(t, tc.want, got.Outcome, "hangup cause %q must map to outcome %q", tc.hangupCause, tc.want)
		})
	}
}

// TestCallEventSubscriber_DuplicateAnswered_IdempotentAcks asserts the
// subscriber treats a second answered event as a no-op (no error
// propagated to the bus). NATS at-least-once means duplicates happen on
// reconnect; the subscriber must NOT NAK them.
func TestCallEventSubscriber_DuplicateAnswered_IdempotentAcks(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	fsm := &fakeFSM{
		// First call succeeds (writes the call into the state); second
		// call returns the canonical FSM "already in target state"
		// sentinel — the subscriber must ack instead of NAK.
		startedErr: nil,
	}
	lookup := newFakeCallLookup()
	sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

	tenantID := uuid.New()
	operatorID := uuid.New()
	callID := uuid.New()
	lookup.bind(tenantID, callID, operatorID)

	ev := newAnsweredEvent(tenantID, callID)
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventAnswer))
	require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)))
	require.Equal(t, int32(1), fsm.startedCount.Load())

	// Inject the sentinel for the redelivery attempt.
	fsm.mu.Lock()
	fsm.startedErr = dialerapi.ErrInvalidTransition
	fsm.mu.Unlock()

	// Second delivery must NOT error (= ack), even though the FSM
	// rejects the duplicate transition.
	require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)))
	require.Equal(t, int32(2), fsm.startedCount.Load())
}

// TestCallEventSubscriber_DuplicateHangup_IdempotentAcks mirrors the
// duplicate-answered case for hangup events.
func TestCallEventSubscriber_DuplicateHangup_IdempotentAcks(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	fsm := &fakeFSM{}
	lookup := newFakeCallLookup()
	sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

	tenantID := uuid.New()
	operatorID := uuid.New()
	callID := uuid.New()
	lookup.bind(tenantID, callID, operatorID)

	ev := newHangupEvent(tenantID, callID, "NORMAL_CLEARING")
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventHangup))
	require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)))

	fsm.mu.Lock()
	fsm.endedErr = dialerapi.ErrInvalidTransition
	fsm.mu.Unlock()

	// Redelivery must ack despite the FSM rejecting the duplicate.
	require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)))
	require.Equal(t, int32(2), fsm.endedCount.Load())
}

// TestCallEventSubscriber_TransientFSMError_NaksForRedelivery asserts
// non-sentinel FSM errors are returned to the bus so JetStream
// redelivers (e.g. Redis is briefly down).
func TestCallEventSubscriber_TransientFSMError_NaksForRedelivery(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	transientErr := errors.New("redis: connection refused")
	fsm := &fakeFSM{startedErr: transientErr}
	lookup := newFakeCallLookup()
	sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

	tenantID := uuid.New()
	operatorID := uuid.New()
	callID := uuid.New()
	lookup.bind(tenantID, callID, operatorID)

	ev := newAnsweredEvent(tenantID, callID)
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventAnswer))
	err := bus.Publish(t, subject, marshalEvent(t, ev))
	require.Error(t, err, "transient FSM errors must propagate so JetStream NAKs")
}

// TestCallEventSubscriber_MalformedPayload_DeadLettersGracefully
// asserts a json-unmarshal failure ACKs (returns nil) rather than
// NAKing — poison messages would loop forever on the broker otherwise.
func TestCallEventSubscriber_MalformedPayload_DeadLettersGracefully(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	fsm := &fakeFSM{}
	lookup := newFakeCallLookup()
	sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

	tenantID := uuid.New()
	callID := uuid.New()
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventAnswer))
	garbage := []byte("{not-valid-json")
	require.NoError(t, bus.Publish(t, subject, garbage), "malformed payload must ACK (return nil)")
	require.Zero(t, fsm.startedCount.Load())
	require.Zero(t, fsm.endedCount.Load())
}

// TestCallEventSubscriber_NilTenantID_DeadLetters guards against a
// publisher that forgets to populate TenantID on the payload (sentinel
// uuid.Nil). We ack + skip rather than feed nil into the FSM.
func TestCallEventSubscriber_NilTenantID_DeadLetters(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	fsm := &fakeFSM{}
	lookup := newFakeCallLookup()
	sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

	ev := telapi.ChannelEvent{
		EventID:  uuid.New(),
		TenantID: uuid.Nil, // sentinel — invalid
		CallID:   uuid.New(),
		Type:     telapi.EventAnswer,
	}
	// Subject built with a non-nil tenant so it still matches the
	// wildcard; the validation is on the payload, not the subject.
	subject := telapi.SubjectChannelEventFor(uuid.New(), ev.CallID, string(telapi.EventAnswer))
	require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)), "nil tenant must ACK")
	require.Zero(t, fsm.startedCount.Load())
}

// TestCallEventSubscriber_UnknownEventType_SkipsSilently asserts events
// the subscriber doesn't route (DTMF / RECORD_STOP / bridge / unbridge
// / dialing) are ack'd without invoking the FSM.
func TestCallEventSubscriber_UnknownEventType_SkipsSilently(t *testing.T) {
	t.Parallel()

	cases := []telapi.ChannelEventType{
		telapi.EventDialing,
		telapi.EventBridge,
		telapi.EventUnbridge,
		telapi.EventDTMF,
		telapi.EventRecordStop,
	}
	for _, kind := range cases {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			bus := newFakeBus()
			fsm := &fakeFSM{}
			lookup := newFakeCallLookup()
			sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
			require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

			tenantID := uuid.New()
			callID := uuid.New()
			ev := telapi.ChannelEvent{
				EventID:  uuid.New(),
				TenantID: tenantID,
				CallID:   callID,
				Type:     kind,
			}
			subject := telapi.SubjectChannelEventFor(tenantID, callID, string(kind))
			require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)))
			require.Zero(t, fsm.startedCount.Load())
			require.Zero(t, fsm.endedCount.Load())
		})
	}
}

// TestCallEventSubscriber_OperatorLookupNotFound_DeadLetters asserts
// that a missing (tenant, call) → operator binding is treated as a
// permanent error (ack + log), not a transient retry-worthy fault. A
// call without an operator row in PG cannot meaningfully transition
// any FSM, so spinning JetStream for it is pure waste.
func TestCallEventSubscriber_OperatorLookupNotFound_DeadLetters(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	fsm := &fakeFSM{}
	lookup := newFakeCallLookup()
	sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

	tenantID := uuid.New()
	callID := uuid.New()
	// Intentionally skip lookup.bind — the lookup returns
	// ErrOperatorNotFound.
	ev := newAnsweredEvent(tenantID, callID)
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventAnswer))
	require.NoError(t, bus.Publish(t, subject, marshalEvent(t, ev)), "operator-not-found must ACK")
	require.Zero(t, fsm.startedCount.Load())
}

// TestCallEventSubscriber_OperatorLookupTransientError_NaksForRedelivery
// guards the opposite path: a transient DB outage on the lookup side
// must NAK so JetStream retries.
func TestCallEventSubscriber_OperatorLookupTransientError_NaksForRedelivery(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	fsm := &fakeFSM{}
	lookup := newFakeCallLookup()
	lookup.err = errors.New("pg: connection refused")
	sub := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, sub.Subscribe(t.Context(), bus, "test-queue"))

	tenantID := uuid.New()
	callID := uuid.New()
	ev := newAnsweredEvent(tenantID, callID)
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventAnswer))
	err := bus.Publish(t, subject, marshalEvent(t, ev))
	require.Error(t, err, "transient lookup errors must NAK for redelivery")
	require.Zero(t, fsm.startedCount.Load())
}

// TestCallEventSubscriber_NilFSMPanics is the wiring-contract guard.
func TestCallEventSubscriber_NilFSMPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		dialernats.NewCallEventSubscriber(nil, newFakeCallLookup(), zap.NewNop())
	})
}

// TestCallEventSubscriber_NilLookupPanics is the wiring-contract guard
// for the call→operator lookup.
func TestCallEventSubscriber_NilLookupPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		dialernats.NewCallEventSubscriber(&fakeFSM{}, nil, zap.NewNop())
	})
}

// TestCallEventSubscriber_NilBusReturnsError asserts a nil bus from a
// wiring mistake surfaces an error rather than panicking.
func TestCallEventSubscriber_NilBusReturnsError(t *testing.T) {
	t.Parallel()
	sub := dialernats.NewCallEventSubscriber(&fakeFSM{}, newFakeCallLookup(), zap.NewNop())
	err := sub.Subscribe(t.Context(), nil, "test-queue")
	require.Error(t, err)
}

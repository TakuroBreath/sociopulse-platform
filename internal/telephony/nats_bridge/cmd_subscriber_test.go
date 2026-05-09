package nats_bridge //nolint:revive // package name mirrors the module's filesystem path

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	telapi "github.com/sociopulse/platform/internal/telephony/api"
)

// --- test doubles -----------------------------------------------------------

// fakeChecker is a tiny stub for the idempotencyChecker the cmd subscriber
// depends on. The cmd_subscriber takes the checker via the interface seam
// declared in cmd_subscriber.go so tests can drive each case independently
// (new / duplicate / Redis-down) without a miniredis instance.
type fakeChecker struct {
	mu  sync.Mutex
	got []string

	// nextOK / nextErr drive the behaviour. Tests set them per-call.
	nextOK  bool
	nextErr error
}

func (f *fakeChecker) MarkSeen(_ context.Context, commandID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, commandID)
	if f.nextErr != nil {
		return false, f.nextErr
	}
	return f.nextOK, nil
}

// fakeDispatcher records every dispatch the cmd subscriber attempts. It
// implements poolDispatcher (the narrow interface the subscriber depends
// on) so a test can drive Originate / Hangup / MixMonitorStart /
// MixMonitorStop in isolation, without standing up the real esl pool.
type fakeDispatcher struct {
	mu sync.Mutex

	originateCalls []originateCall
	hangupCalls    []hangupCall
	mmStartCalls   []mmStartCall
	mmStopCalls    []mmStopCall

	// returnErr, when non-nil, is returned from every method below — used
	// to drive the "pool dispatch failed → NACK" branch.
	returnErr error
}

type originateCall struct {
	node string
	cmd  telapi.OriginateCommand
}

type hangupCall struct {
	node, callID, cause string
}

type mmStartCall struct {
	node, callID, path string
}

type mmStopCall struct {
	node, callID string
}

func (f *fakeDispatcher) Originate(_ context.Context, node string, cmd telapi.OriginateCommand) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.originateCalls = append(f.originateCalls, originateCall{node: node, cmd: cmd})
	return f.returnErr
}

func (f *fakeDispatcher) Hangup(_ context.Context, node, callID, cause string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangupCalls = append(f.hangupCalls, hangupCall{node: node, callID: callID, cause: cause})
	return f.returnErr
}

func (f *fakeDispatcher) MixMonitorStart(_ context.Context, node, callID, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mmStartCalls = append(f.mmStartCalls, mmStartCall{node: node, callID: callID, path: path})
	return f.returnErr
}

func (f *fakeDispatcher) MixMonitorStop(_ context.Context, node, callID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mmStopCalls = append(f.mmStopCalls, mmStopCall{node: node, callID: callID})
	return f.returnErr
}

// --- helpers ----------------------------------------------------------------

func mustEnvelope(t *testing.T, env commandEnvelope) []byte {
	t.Helper()
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return b
}

func newSubscriberUnderTest(t *testing.T, fd *fakeDispatcher, fc *fakeChecker) (*cmdSubscriber, *Metrics) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := RegisterMetrics(reg)
	sub := newCmdSubscriber(nil, fd, fc, m, zap.NewNop())
	return sub, m
}

// --- tests ------------------------------------------------------------------

// TestCmdSubscriber_DispatchesOriginateToPool covers the happy path: a
// well-formed originate envelope flows from the bus into pool.Originate
// with the right node + args.
func TestCmdSubscriber_DispatchesOriginateToPool(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{}
	fc := &fakeChecker{nextOK: true}
	sub, metrics := newSubscriberUnderTest(t, fd, fc)

	cmdID := uuid.New()
	tenantID := uuid.New()
	callID := uuid.New()
	args := telapi.OriginateCommand{
		CommandID:      cmdID,
		TenantID:       tenantID,
		CallID:         callID,
		OperatorExt:    "1001",
		Number:         "+15551234567",
		TrunkID:        "primary",
		FSNode:         "fs-a:8021",
		PromptURL:      "",
		RecordingPath:  "/var/recordings/x.wav",
		CallerID:       "+15558880000",
		DialingTimeout: 30 * time.Second,
	}
	rawArgs, err := json.Marshal(args)
	require.NoError(t, err)

	payload := mustEnvelope(t, commandEnvelope{
		CommandID: cmdID.String(),
		Kind:      kindOriginate,
		TenantID:  tenantID.String(),
		CallID:    callID.String(),
		Node:      "fs-a:8021",
		Args:      rawArgs,
	})

	err = sub.handle(telapi.SubjectCommandFor(tenantID, callID), payload)
	require.NoError(t, err)

	require.Len(t, fd.originateCalls, 1, "pool.Originate must be called once")
	got := fd.originateCalls[0]
	assert.Equal(t, "fs-a:8021", got.node)
	assert.Equal(t, args.OperatorExt, got.cmd.OperatorExt)
	assert.Equal(t, args.Number, got.cmd.Number)
	assert.Equal(t, args.TrunkID, got.cmd.TrunkID)

	require.Equal(t, []string{cmdID.String()}, fc.got, "idempotency checker called with command_id")

	// CommandsReceived{kind=originate}=1.
	assert.InDelta(t, 1.0,
		testutil.ToFloat64(metrics.CommandsReceived.WithLabelValues(kindOriginate)),
		1e-9)
}

// TestCmdSubscriber_DispatchesHangup covers the hangup branch with an
// args object carrying the cause field.
func TestCmdSubscriber_DispatchesHangup(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{}
	fc := &fakeChecker{nextOK: true}
	sub, _ := newSubscriberUnderTest(t, fd, fc)

	tenantID := uuid.New()
	callID := uuid.New()
	cmdID := uuid.New()
	rawArgs, _ := json.Marshal(struct {
		Cause string `json:"cause"`
	}{Cause: "USER_BUSY"})

	payload := mustEnvelope(t, commandEnvelope{
		CommandID: cmdID.String(),
		Kind:      kindHangup,
		TenantID:  tenantID.String(),
		CallID:    callID.String(),
		Node:      "fs-b:8021",
		Args:      rawArgs,
	})

	err := sub.handle(telapi.SubjectCommandFor(tenantID, callID), payload)
	require.NoError(t, err)

	require.Len(t, fd.hangupCalls, 1)
	assert.Equal(t, "fs-b:8021", fd.hangupCalls[0].node)
	assert.Equal(t, callID.String(), fd.hangupCalls[0].callID)
	assert.Equal(t, "USER_BUSY", fd.hangupCalls[0].cause)
}

// TestCmdSubscriber_DispatchesMixMonitorStart covers the mixmonitor.start
// branch with an args object carrying the recording path.
func TestCmdSubscriber_DispatchesMixMonitorStart(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{}
	fc := &fakeChecker{nextOK: true}
	sub, _ := newSubscriberUnderTest(t, fd, fc)

	tenantID := uuid.New()
	callID := uuid.New()
	cmdID := uuid.New()
	rawArgs, _ := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: "/var/rec/x.wav"})

	payload := mustEnvelope(t, commandEnvelope{
		CommandID: cmdID.String(),
		Kind:      kindMixmonitorStart,
		TenantID:  tenantID.String(),
		CallID:    callID.String(),
		Node:      "fs-a:8021",
		Args:      rawArgs,
	})

	err := sub.handle(telapi.SubjectCommandFor(tenantID, callID), payload)
	require.NoError(t, err)

	require.Len(t, fd.mmStartCalls, 1)
	assert.Equal(t, "/var/rec/x.wav", fd.mmStartCalls[0].path)
}

// TestCmdSubscriber_DispatchesMixMonitorStop covers the no-args mixmonitor.stop
// branch.
func TestCmdSubscriber_DispatchesMixMonitorStop(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{}
	fc := &fakeChecker{nextOK: true}
	sub, _ := newSubscriberUnderTest(t, fd, fc)

	tenantID := uuid.New()
	callID := uuid.New()
	cmdID := uuid.New()

	payload := mustEnvelope(t, commandEnvelope{
		CommandID: cmdID.String(),
		Kind:      kindMixmonitorStop,
		TenantID:  tenantID.String(),
		CallID:    callID.String(),
		Node:      "fs-a:8021",
	})

	err := sub.handle(telapi.SubjectCommandFor(tenantID, callID), payload)
	require.NoError(t, err)

	require.Len(t, fd.mmStopCalls, 1)
}

// TestCmdSubscriber_SkipsDuplicateCommand asserts that when the idempotency
// guard says "already seen", pool.Originate is NOT invoked AND the handler
// returns nil so the broker acks (NACKing a duplicate would loop forever).
func TestCmdSubscriber_SkipsDuplicateCommand(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{}
	fc := &fakeChecker{nextOK: false} // duplicate
	sub, metrics := newSubscriberUnderTest(t, fd, fc)

	tenantID := uuid.New()
	callID := uuid.New()
	cmdID := uuid.New()
	rawArgs, _ := json.Marshal(telapi.OriginateCommand{CommandID: cmdID, TenantID: tenantID, CallID: callID})

	payload := mustEnvelope(t, commandEnvelope{
		CommandID: cmdID.String(),
		Kind:      kindOriginate,
		TenantID:  tenantID.String(),
		CallID:    callID.String(),
		Node:      "fs-a:8021",
		Args:      rawArgs,
	})

	err := sub.handle(telapi.SubjectCommandFor(tenantID, callID), payload)
	require.NoError(t, err, "handler MUST return nil on duplicate so broker acks")

	require.Empty(t, fd.originateCalls, "pool MUST NOT be called on duplicate")

	// CommandsRejected{reason=duplicate}=1.
	assert.InDelta(t, 1.0,
		testutil.ToFloat64(metrics.CommandsRejected.WithLabelValues(rejectReasonDuplicate)),
		1e-9)
}

// TestCmdSubscriber_NACKsOnPoolError asserts a pool dispatch error
// propagates out of the handler so the bus NAKs and redelivers.
func TestCmdSubscriber_NACKsOnPoolError(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{returnErr: errors.New("esl: connection reset")}
	fc := &fakeChecker{nextOK: true}
	sub, metrics := newSubscriberUnderTest(t, fd, fc)

	tenantID := uuid.New()
	callID := uuid.New()
	cmdID := uuid.New()
	rawArgs, _ := json.Marshal(telapi.OriginateCommand{CommandID: cmdID, TenantID: tenantID, CallID: callID})

	payload := mustEnvelope(t, commandEnvelope{
		CommandID: cmdID.String(),
		Kind:      kindOriginate,
		TenantID:  tenantID.String(),
		CallID:    callID.String(),
		Node:      "fs-a:8021",
		Args:      rawArgs,
	})

	err := sub.handle(telapi.SubjectCommandFor(tenantID, callID), payload)
	require.Error(t, err, "pool error must surface so bus NAKs")

	assert.InDelta(t, 1.0,
		testutil.ToFloat64(metrics.CommandsRejected.WithLabelValues(rejectReasonPoolError)),
		1e-9)
}

// TestCmdSubscriber_NACKsOnIdempotencyRedisFailure ensures a Redis-side
// idempotency failure surfaces as a handler error so the broker
// redelivers — silent dedup-failure would let commands be dropped.
func TestCmdSubscriber_NACKsOnIdempotencyRedisFailure(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{}
	fc := &fakeChecker{nextErr: errors.New("redis: connection refused")}
	sub, metrics := newSubscriberUnderTest(t, fd, fc)

	tenantID := uuid.New()
	callID := uuid.New()
	cmdID := uuid.New()
	rawArgs, _ := json.Marshal(telapi.OriginateCommand{CommandID: cmdID, TenantID: tenantID, CallID: callID})

	payload := mustEnvelope(t, commandEnvelope{
		CommandID: cmdID.String(),
		Kind:      kindOriginate,
		TenantID:  tenantID.String(),
		CallID:    callID.String(),
		Node:      "fs-a:8021",
		Args:      rawArgs,
	})

	err := sub.handle(telapi.SubjectCommandFor(tenantID, callID), payload)
	require.Error(t, err, "redis-down on idempotency must surface so bus NAKs")
	require.Empty(t, fd.originateCalls, "pool MUST NOT be called when idempotency check fails")

	assert.InDelta(t, 1.0,
		testutil.ToFloat64(metrics.CommandsRejected.WithLabelValues(rejectReasonIdempotencyError)),
		1e-9)
}

// TestCmdSubscriber_AcksOnMalformedJSON proves a malformed envelope is a
// permanent error: the handler returns nil so the broker acks (no
// redelivery loop) and we tick the rejected metric for observability.
func TestCmdSubscriber_AcksOnMalformedJSON(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{}
	fc := &fakeChecker{nextOK: true}
	sub, metrics := newSubscriberUnderTest(t, fd, fc)

	err := sub.handle("tenant.x.telephony.cmd.y", []byte("not json {{{"))
	require.NoError(t, err, "malformed payloads MUST ack — they will never parse")

	require.Empty(t, fd.originateCalls, "pool MUST NOT be called on malformed envelope")

	assert.InDelta(t, 1.0,
		testutil.ToFloat64(metrics.CommandsRejected.WithLabelValues(rejectReasonMalformed)),
		1e-9)
}

// TestCmdSubscriber_AcksOnUnknownKind asserts an unknown kind is treated
// the same way as malformed: ack + tick rejected metric, no NACK loop.
func TestCmdSubscriber_AcksOnUnknownKind(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{}
	fc := &fakeChecker{nextOK: true}
	sub, metrics := newSubscriberUnderTest(t, fd, fc)

	tenantID := uuid.New()
	callID := uuid.New()
	cmdID := uuid.New()

	payload := mustEnvelope(t, commandEnvelope{
		CommandID: cmdID.String(),
		Kind:      "unknown.command.kind",
		TenantID:  tenantID.String(),
		CallID:    callID.String(),
		Node:      "fs-a:8021",
	})

	err := sub.handle(telapi.SubjectCommandFor(tenantID, callID), payload)
	require.NoError(t, err)

	require.Empty(t, fd.originateCalls)
	require.Empty(t, fd.hangupCalls)
	require.Empty(t, fd.mmStartCalls)
	require.Empty(t, fd.mmStopCalls)

	assert.InDelta(t, 1.0,
		testutil.ToFloat64(metrics.CommandsRejected.WithLabelValues(rejectReasonUnknownKind)),
		1e-9)
}

// TestCmdSubscriber_RejectsMalformedHangupArgs covers the case where the
// envelope is valid but the typed args sub-DTO will not decode into the
// hangup args shape. Same ack-with-rejected-metric semantics as the outer
// malformed-envelope case — a permanently undecodable args payload would
// otherwise loop on every redelivery.
func TestCmdSubscriber_RejectsMalformedHangupArgs(t *testing.T) {
	t.Parallel()

	fd := &fakeDispatcher{}
	fc := &fakeChecker{nextOK: true}
	sub, metrics := newSubscriberUnderTest(t, fd, fc)

	tenantID := uuid.New()
	callID := uuid.New()
	cmdID := uuid.New()

	payload := mustEnvelope(t, commandEnvelope{
		CommandID: cmdID.String(),
		Kind:      kindHangup,
		TenantID:  tenantID.String(),
		CallID:    callID.String(),
		Node:      "fs-a:8021",
		// Args is a non-object literal — the {cause: string} decoder
		// rejects it.
		Args: []byte(`true`),
	})

	err := sub.handle(telapi.SubjectCommandFor(tenantID, callID), payload)
	require.NoError(t, err, "args parse failure ack: malformed forever")

	require.Empty(t, fd.hangupCalls)
	assert.InDelta(t, 1.0,
		testutil.ToFloat64(metrics.CommandsRejected.WithLabelValues(rejectReasonMalformed)),
		1e-9)
}

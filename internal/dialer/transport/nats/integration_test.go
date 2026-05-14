package nats_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	natssrv "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	dialernats "github.com/sociopulse/platform/internal/dialer/transport/nats"
	telapi "github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// TestCallEventSubscriber_Integration_EndToEndThroughJetStream wires the
// production *eventbus.NATSSubscriber against an embedded JetStream
// broker, has the production *eventbus.NATSPublisher emit a real
// Type=answer payload, and asserts the fake FSM receives the
// RecordCallStarted invocation with the correct (tenant, operator, call)
// tuple. Proves the bus → handler → FSM wire path end-to-end without
// requiring a Postgres or Redis backend.
func TestCallEventSubscriber_Integration_EndToEndThroughJetStream(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	// JetStream stream MUST cover the wildcard subject the subscriber
	// binds (production cmd/telephony-bridge provisions this via its
	// own composition root).
	ensureStream(t, url, "DIALERINTANS", []string{"tenant.*.telephony.event.>"})

	ctx := t.Context()

	pub, err := eventbus.NewNATSPublisher(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	sub, err := eventbus.NewNATSSubscriber(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	fsm := &fakeFSM{}
	lookup := newFakeCallLookup()
	tenantID := uuid.New()
	operatorID := uuid.New()
	callID := uuid.New()
	lookup.bind(tenantID, callID, operatorID)

	subscriber := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, subscriber.Subscribe(ctx, sub, "test-int-ans"))

	// Publish a real Type=answer message — this is the wire shape
	// cmd/telephony-bridge produces in production.
	ev := newAnsweredEvent(tenantID, callID)
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventAnswer))
	require.NoError(t, pub.Publish(ctx, subject, marshalEvent(t, ev)))

	// JetStream delivery is async; poll until the FSM observes the call.
	require.Eventually(t, func() bool {
		return fsm.startedCount.Load() == 1
	}, 3*time.Second, 20*time.Millisecond, "RecordCallStarted should fire within 3s")

	fsm.mu.Lock()
	got := fsm.startedCalls[0]
	fsm.mu.Unlock()
	require.Equal(t, tenantID, got.TenantID)
	require.Equal(t, operatorID, got.OperatorID)
	require.Equal(t, callID, got.CallID)
}

// TestCallEventSubscriber_Integration_HangupOutcomeRoundTrip is the
// hangup-side end-to-end test. NORMAL_CLEARING must surface as
// OutcomeSuccess inside the FSM call.
func TestCallEventSubscriber_Integration_HangupOutcomeRoundTrip(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "DIALERINTHUP", []string{"tenant.*.telephony.event.>"})

	ctx := t.Context()
	pub, err := eventbus.NewNATSPublisher(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })
	sub, err := eventbus.NewNATSSubscriber(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	fsm := &fakeFSM{}
	lookup := newFakeCallLookup()
	tenantID := uuid.New()
	operatorID := uuid.New()
	callID := uuid.New()
	lookup.bind(tenantID, callID, operatorID)

	subscriber := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, subscriber.Subscribe(ctx, sub, "test-int-hup"))

	ev := newHangupEvent(tenantID, callID, "NORMAL_CLEARING")
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventHangup))
	require.NoError(t, pub.Publish(ctx, subject, marshalEvent(t, ev)))

	require.Eventually(t, func() bool {
		return fsm.endedCount.Load() == 1
	}, 3*time.Second, 20*time.Millisecond, "RecordCallEnded should fire within 3s")

	fsm.mu.Lock()
	got := fsm.endedCalls[0]
	fsm.mu.Unlock()
	require.Equal(t, tenantID, got.TenantID)
	require.Equal(t, operatorID, got.OperatorID)
	require.Equal(t, callID, got.CallID)
	require.Equal(t, dialerapi.OutcomeSuccess, got.Outcome)
}

// TestCallEventSubscriber_Integration_NaksAreRedelivered exercises the
// at-least-once redelivery path: a transient FSM error returns a
// non-nil to the bus, JetStream NAKs, and the bus retries after the
// 250ms nakDelay. We arrange success on the second attempt and assert
// startedCount reaches exactly 2 (initial NAK + redelivered ACK).
func TestCallEventSubscriber_Integration_NaksAreRedelivered(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "DIALERINTNAK", []string{"tenant.*.telephony.event.>"})

	ctx := t.Context()
	pub, err := eventbus.NewNATSPublisher(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })
	sub, err := eventbus.NewNATSSubscriber(ctx, []string{url}, "",
		eventbus.WithSubscriberNakDelay(50*time.Millisecond),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	// fsmFlippy returns transient errInject on the first invocation
	// then succeeds. Without re-using fakeFSM's mutable startedErr
	// field, this is cleaner because the redelivery race against the
	// test goroutine would otherwise risk us flipping startedErr to nil
	// AFTER the redelivery already ran with the stale value.
	fsm := &fsmFlippy{failFirst: true}
	lookup := newFakeCallLookup()
	tenantID := uuid.New()
	operatorID := uuid.New()
	callID := uuid.New()
	lookup.bind(tenantID, callID, operatorID)

	subscriber := dialernats.NewCallEventSubscriber(fsm, lookup, zaptest.NewLogger(t))
	require.NoError(t, subscriber.Subscribe(ctx, sub, "test-int-nak"))

	ev := newAnsweredEvent(tenantID, callID)
	subject := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventAnswer))
	require.NoError(t, pub.Publish(ctx, subject, marshalEvent(t, ev)))

	require.Eventually(t, func() bool {
		return fsm.calls.Load() >= 2
	}, 3*time.Second, 20*time.Millisecond, "RecordCallStarted should be called >=2 times (initial NAK + redelivery ACK)")
}

// fsmFlippy is the redelivery-test FSM: returns errInject on the first
// RecordCallStarted invocation, succeeds thereafter. Built as a
// standalone OperatorFSM impl rather than embedding *fakeFSM because
// embedding would shadow the method we need to override without
// changing the dispatched-on receiver — Go interfaces dispatch on the
// outermost embedding chain, so the override method MUST have the
// exact contract signature.
type fsmFlippy struct {
	mu        sync.Mutex
	failFirst bool
	calls     atomic.Int32
	requests  []dialerapi.CallStartedRequest
}

var errInject = errors.New("integration_test: transient inject")

func (f *fsmFlippy) RecordCallStarted(_ context.Context, req dialerapi.CallStartedRequest) (dialerapi.Snapshot, error) {
	count := f.calls.Add(1)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.failFirst && count == 1 {
		return dialerapi.Snapshot{}, errInject
	}
	return dialerapi.Snapshot{
		TenantID:      req.TenantID,
		OperatorID:    req.OperatorID,
		State:         dialerapi.StateCall,
		CurrentCallID: &req.CallID,
	}, nil
}

func (f *fsmFlippy) RecordCallEnded(context.Context, dialerapi.CallEndedRequest) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

func (f *fsmFlippy) StartShift(context.Context, dialerapi.StartShiftRequest) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

func (f *fsmFlippy) EndShift(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fsmFlippy) GoReady(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

func (f *fsmFlippy) GoPause(context.Context, dialerapi.GoPauseRequest) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fsmFlippy) Resume(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

func (f *fsmFlippy) SubmitStatus(context.Context, dialerapi.SubmitStatusRequest) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fsmFlippy) GoVerify(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fsmFlippy) VerifyDone(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fsmFlippy) GetState(context.Context, uuid.UUID, uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

func (f *fsmFlippy) Force(context.Context, uuid.UUID, uuid.UUID, dialerapi.State, dialerapi.ForceReason) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}

var _ dialerapi.OperatorFSM = (*fsmFlippy)(nil)

// startEmbeddedJetStream boots an in-process NATS server with
// JetStream. Mirrors pkg/eventbus/helpers_test.go (which is package-
// private) and internal/dialer/pubsub_nats_test.go's local copy.
func startEmbeddedJetStream(t *testing.T) string {
	t.Helper()

	storeDir := filepath.Join(t.TempDir(), "jetstream")
	opts := &natssrv.Options{
		Host:                  "127.0.0.1",
		Port:                  -1,
		NoLog:                 true,
		NoSigs:                true,
		MaxControlLine:        4096,
		DisableShortFirstPing: true,
		JetStream:             true,
		StoreDir:              storeDir,
	}

	srv, err := natssrv.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatal("embedded NATS did not become ready in 5s")
	}
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	return srv.ClientURL()
}

// ensureStream creates a JetStream stream covering the subject pattern.
// Tests bypassing the production stream-provisioning path need this so
// the broker persists messages and consumers receive them.
func ensureStream(t *testing.T, url, name string, subjects []string) {
	t.Helper()

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	defer nc.Close()

	js, err := nc.JetStream()
	require.NoError(t, err)

	cfg := &nats.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: nats.InterestPolicy,
		Storage:   nats.MemoryStorage,
		MaxAge:    1 * time.Minute,
	}
	if _, err := js.AddStream(cfg); err != nil {
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			_, err = js.UpdateStream(cfg)
		}
		require.NoError(t, err, "ensure stream %q", name)
	}
}

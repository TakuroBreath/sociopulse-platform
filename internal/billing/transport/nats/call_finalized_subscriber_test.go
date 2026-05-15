package nats_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	billingnats "github.com/sociopulse/platform/internal/billing/transport/nats"
	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// fakeHook records OnCallFinalized invocations + simulates errors.
type fakeHook struct {
	mu  sync.Mutex
	in  []billingapi.CallCostInput
	err error
}

func (f *fakeHook) OnCallFinalized(_ context.Context, in billingapi.CallCostInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.in = append(f.in, in)
	return nil
}

func (f *fakeHook) calls() []billingapi.CallCostInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]billingapi.CallCostInput(nil), f.in...)
}

// fakeBus is a noop eventbus.Subscriber for Subscribe smoke tests.
// It captures the last Subscribe call's args and exposes the handler so
// tests can drive the dispatch path synchronously without spinning up
// a real bus.
type fakeBus struct {
	err error
	// recorded args from the last Subscribe call.
	subject string
	queue   string
	handler func(string, []byte) error
}

func (b *fakeBus) Subscribe(_ context.Context, subject, queue string, h func(string, []byte) error) error {
	b.subject = subject
	b.queue = queue
	b.handler = h
	return b.err
}

var _ eventbus.Subscriber = (*fakeBus)(nil)

func TestSubscribe_RegistersWildcardSubjectAndQueue(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	s := billingnats.NewCallFinalizedSubscriber(&fakeHook{}, zap.NewNop())
	require.NoError(t, s.Subscribe(context.Background(), bus))
	require.Equal(t, billingnats.SubscribeSubject, bus.subject)
	require.Equal(t, billingnats.QueueGroup, bus.queue)
	require.NotNil(t, bus.handler)
}

func TestSubscribe_NilBus_ReturnsError(t *testing.T) {
	t.Parallel()
	s := billingnats.NewCallFinalizedSubscriber(&fakeHook{}, zap.NewNop())
	require.Error(t, s.Subscribe(context.Background(), nil))
}

func TestSubscribe_BusError_Propagated(t *testing.T) {
	t.Parallel()
	busErr := errors.New("simulated bus failure")
	bus := &fakeBus{err: busErr}
	s := billingnats.NewCallFinalizedSubscriber(&fakeHook{}, zap.NewNop())
	err := s.Subscribe(context.Background(), bus)
	require.Error(t, err)
	require.ErrorIs(t, err, busErr)
}

func TestHandle_DecodesPayload_AndDispatches(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	hook := &fakeHook{}
	s := billingnats.NewCallFinalizedSubscriber(hook, zap.NewNop())
	require.NoError(t, s.Subscribe(context.Background(), bus))

	finalizedAt := time.Date(2026, 5, 12, 18, 1, 23, 0, time.UTC)
	ev := dialerapi.CallFinalizedEvent{
		CallID:       uuid.New(),
		TenantID:     uuid.New(),
		ProjectID:    uuid.New(),
		OperatorID:   uuid.New(),
		RespondentID: uuid.New(),
		TrunkUsed:    "mtt-msk-1",
		DurationSec:  60,
		Status:       "success",
		StorageBytes: 1 << 20,
		FinalizedAt:  finalizedAt.Unix(),
	}
	payload, err := json.Marshal(ev)
	require.NoError(t, err)

	// Invoke the captured handler directly to exercise decode + dispatch.
	require.NoError(t, bus.handler("tenant."+ev.TenantID.String()+".dialer.call.finalized", payload))

	got := hook.calls()
	require.Len(t, got, 1)
	require.Equal(t, ev.CallID, got[0].CallID)
	require.Equal(t, ev.TenantID, got[0].TenantID)
	require.Equal(t, ev.ProjectID, got[0].ProjectID)
	require.Equal(t, ev.TrunkUsed, got[0].TrunkUsed)
	require.Equal(t, int32(60), got[0].DurationSec)
	require.Equal(t, "success", got[0].Status)
	require.Equal(t, int64(1<<20), got[0].StorageBytes)
	require.True(t, finalizedAt.Equal(got[0].FinalizedAt),
		"FinalizedAt must round-trip through unix seconds: got %s, want %s",
		got[0].FinalizedAt, finalizedAt)
}

func TestHandle_BadJSON_ReturnsError(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	s := billingnats.NewCallFinalizedSubscriber(&fakeHook{}, zap.NewNop())
	require.NoError(t, s.Subscribe(context.Background(), bus))
	require.Error(t, bus.handler("any.subject", []byte("not-json")))
}

func TestHandle_MissingCallID_ReturnsError(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	hook := &fakeHook{}
	s := billingnats.NewCallFinalizedSubscriber(hook, zap.NewNop())
	require.NoError(t, s.Subscribe(context.Background(), bus))

	payload, err := json.Marshal(dialerapi.CallFinalizedEvent{
		TenantID:    uuid.New(),
		Status:      "success",
		DurationSec: 60,
		FinalizedAt: time.Now().Unix(),
	})
	require.NoError(t, err)
	require.Error(t, bus.handler("any.subject", payload))
	require.Empty(t, hook.calls(), "hook must not be invoked when call_id is missing")
}

func TestHandle_MissingTenantID_ReturnsError(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	hook := &fakeHook{}
	s := billingnats.NewCallFinalizedSubscriber(hook, zap.NewNop())
	require.NoError(t, s.Subscribe(context.Background(), bus))

	payload, err := json.Marshal(dialerapi.CallFinalizedEvent{
		CallID:      uuid.New(),
		Status:      "success",
		DurationSec: 60,
		FinalizedAt: time.Now().Unix(),
	})
	require.NoError(t, err)
	require.Error(t, bus.handler("any.subject", payload))
	require.Empty(t, hook.calls())
}

func TestHandle_HookError_TriggersRedelivery(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	hookErr := errors.New("simulated hook failure")
	hook := &fakeHook{err: hookErr}
	s := billingnats.NewCallFinalizedSubscriber(hook, zap.NewNop())
	require.NoError(t, s.Subscribe(context.Background(), bus))

	payload, err := json.Marshal(dialerapi.CallFinalizedEvent{
		CallID: uuid.New(), TenantID: uuid.New(),
		Status: "success", DurationSec: 60, FinalizedAt: time.Now().Unix(),
	})
	require.NoError(t, err)

	err = bus.handler("any.subject", payload)
	require.Error(t, err)
	require.ErrorIs(t, err, hookErr) // wraps with %w
}

func TestNewCallFinalizedSubscriber_PanicsOnNilHook(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t,
		"billing/transport/nats: NewCallFinalizedSubscriber: hook must be non-nil",
		func() { _ = billingnats.NewCallFinalizedSubscriber(nil, zap.NewNop()) })
}

func TestNewCallFinalizedSubscriber_NilLoggerDefaultsToNop(t *testing.T) {
	t.Parallel()
	// Nil logger MUST NOT panic — the canonical project pattern (see
	// dialer/transport/nats.NewCallEventSubscriber) defaults nil to a
	// no-op zap logger so tests don't have to construct one.
	require.NotPanics(t, func() {
		_ = billingnats.NewCallFinalizedSubscriber(&fakeHook{}, nil)
	})
}

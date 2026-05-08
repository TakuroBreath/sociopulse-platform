package router_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/router"
	telephonyapi "github.com/sociopulse/platform/internal/telephony/api"
)

// TestMain enforces goroutine quiescence on package exit. The Router
// itself spawns no goroutines (Subscribe forwards to the underlying
// EventConsumer's delivery goroutine, which lives in telephony's
// implementation), but goleak protects against a future regression
// that adds one without a Stop hook.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakePublisher is a recording telephony.api.CommandPublisher used to
// assert the translation layer's output. Each method appends the
// received command to the corresponding slice and returns its
// configured error (default nil).
type fakePublisher struct {
	mu sync.Mutex

	originateCmds []telephonyapi.OriginateCommand
	hangupCmds    []telephonyapi.HangupCommand

	originateErr error
	hangupErr    error
}

func (f *fakePublisher) Originate(_ context.Context, cmd telephonyapi.OriginateCommand) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.originateCmds = append(f.originateCmds, cmd)
	return f.originateErr
}

func (f *fakePublisher) Hangup(_ context.Context, cmd telephonyapi.HangupCommand) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangupCmds = append(f.hangupCmds, cmd)
	return f.hangupErr
}

// The remaining CommandPublisher methods are not exercised by the
// dialer Router but satisfy the interface so cfg.Publisher type-checks.
// Each returns a panicking error to make accidental invocation loud.
func (f *fakePublisher) Mixmonitor(_ context.Context, _ telephonyapi.MixmonitorCommand) error {
	return errors.New("fakePublisher.Mixmonitor: not implemented in dialer router test")
}

func (f *fakePublisher) Play(_ context.Context, _ telephonyapi.PlayCommand) error {
	return errors.New("fakePublisher.Play: not implemented in dialer router test")
}

func (f *fakePublisher) CreateUser(_ context.Context, _ telephonyapi.CreateUserCommand) error {
	return errors.New("fakePublisher.CreateUser: not implemented in dialer router test")
}

func (f *fakePublisher) DeleteUser(_ context.Context, _ telephonyapi.DeleteUserCommand) error {
	return errors.New("fakePublisher.DeleteUser: not implemented in dialer router test")
}

func (f *fakePublisher) originated() []telephonyapi.OriginateCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telephonyapi.OriginateCommand(nil), f.originateCmds...)
}

func (f *fakePublisher) hungup() []telephonyapi.HangupCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telephonyapi.HangupCommand(nil), f.hangupCmds...)
}

// fakeConsumer is a controllable telephony.api.EventConsumer. Subscribe
// captures the registered handler in handler; tests call Push to
// invoke the handler synchronously. unsubscribed reports how many
// times the unsubscribe func has been called (idempotency probe).
type fakeConsumer struct {
	mu sync.Mutex

	handler      telephonyapi.EventHandler
	tenantID     uuid.UUID
	subscribeErr error
	unsubscribed int
}

func (f *fakeConsumer) Subscribe(_ context.Context, tenantID uuid.UUID, h telephonyapi.EventHandler) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	f.handler = h
	f.tenantID = tenantID
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unsubscribed++
	}, nil
}

// Push invokes the captured handler with evt. Returns the handler's
// error so tests can assert it propagates back through the consumer
// surface (which is what NACKs in production).
func (f *fakeConsumer) Push(ctx context.Context, evt telephonyapi.ChannelEvent) error {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	if h == nil {
		return errors.New("fakeConsumer.Push: no handler registered")
	}
	return h(ctx, evt)
}

// newRouterT builds a Router with fresh fakes + a fresh metrics
// registry. Returns the router and the fakes for assertion. The
// metrics object is exposed too so tests can probe individual counters
// via testutil.ToFloat64.
type rig struct {
	r       *router.Router
	pub     *fakePublisher
	con     *fakeConsumer
	metrics *router.Metrics
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pub := &fakePublisher{}
	con := &fakeConsumer{}
	reg := prometheus.NewRegistry()
	metrics := router.RegisterMetrics(reg)
	r, err := router.New(router.Config{
		Publisher: pub,
		Consumer:  con,
		Logger:    zaptest.NewLogger(t),
		Metrics:   metrics,
	})
	require.NoError(t, err)
	return &rig{r: r, pub: pub, con: con, metrics: metrics}
}

// TestNew_RequiresPublisher — Publisher is required; no other slot is.
func TestNew_RequiresPublisher(t *testing.T) {
	t.Parallel()
	_, err := router.New(router.Config{
		Consumer: &fakeConsumer{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Publisher")
}

// TestNew_RequiresConsumer — Consumer is required.
func TestNew_RequiresConsumer(t *testing.T) {
	t.Parallel()
	_, err := router.New(router.Config{
		Publisher: &fakePublisher{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Consumer")
}

// TestNew_Defaults — nil Logger / Clock / Metrics fall back to
// defaults; constructor returns no error.
func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	_, err := router.New(router.Config{
		Publisher: &fakePublisher{},
		Consumer:  &fakeConsumer{},
	})
	require.NoError(t, err)
}

// TestRouter_DialPublishesTranslatedCommand — Dial calls Originate
// once with a translated OriginateCommand. The CommandID is fresh
// (non-nil); the field mapping matches translateOriginate.
func TestRouter_DialPublishesTranslatedCommand(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	tenantID := uuid.New()
	callID := uuid.New()
	req := dialerapi.DialRequest{
		CallID:      callID,
		TenantID:    tenantID,
		OperatorID:  uuid.New(),
		ProjectID:   uuid.New(),
		OperatorExt: "lst_42",
		Phone:       "+79991234567",
		FsNode:      "fs1.example.com:8021",
	}

	require.NoError(t, r.r.Dial(context.Background(), req))

	got := r.pub.originated()
	require.Len(t, got, 1)
	require.Equal(t, callID, got[0].CallID)
	require.Equal(t, tenantID, got[0].TenantID)
	require.Equal(t, "lst_42", got[0].OperatorExt)
	require.Equal(t, "+79991234567", got[0].Number)
	require.Equal(t, "fs1.example.com:8021", got[0].FSNode)
	require.NotEqual(t, uuid.Nil, got[0].CommandID)

	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Dials.WithLabelValues("ok")), 0)
	require.InDelta(t, 0.0, testutil.ToFloat64(r.metrics.Dials.WithLabelValues("error")), 0)
}

// TestRouter_DialFreshCommandIDs — two consecutive Dial calls with
// the same DialRequest land as TWO distinct OriginateCommands with
// DIFFERENT CommandIDs. This is the contract the bridge's idempotency
// layer relies on: a dialer-level retry must not collide with the
// previous attempt's idempotency key.
func TestRouter_DialFreshCommandIDs(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	req := dialerapi.DialRequest{
		CallID:      uuid.New(),
		TenantID:    uuid.New(),
		OperatorExt: "lst_42",
		Phone:       "+79991234567",
		FsNode:      "fs1.example.com:8021",
	}
	require.NoError(t, r.r.Dial(context.Background(), req))
	require.NoError(t, r.r.Dial(context.Background(), req))

	got := r.pub.originated()
	require.Len(t, got, 2)
	require.NotEqual(t, got[0].CommandID, got[1].CommandID)
}

// TestRouter_DialErrorPropagates — a publisher error surfaces
// verbatim and the error metric ticks.
func TestRouter_DialErrorPropagates(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	sentinel := errors.New("bridge offline")
	r.pub.originateErr = sentinel

	err := r.r.Dial(context.Background(), dialerapi.DialRequest{
		CallID:      uuid.New(),
		TenantID:    uuid.New(),
		OperatorExt: "lst_42",
		Phone:       "+79991234567",
		FsNode:      "fs1.example.com:8021",
	})
	require.ErrorIs(t, err, sentinel)
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Dials.WithLabelValues("error")), 0)
	require.InDelta(t, 0.0, testutil.ToFloat64(r.metrics.Dials.WithLabelValues("ok")), 0)
}

// TestRouter_HangupPublishesTranslatedCommand — Hangup forwards
// (callID, reason) translated into HangupCommand with a fresh
// CommandID.
func TestRouter_HangupPublishesTranslatedCommand(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	callID := uuid.New()
	require.NoError(t, r.r.Hangup(context.Background(), callID, "USER_BUSY"))

	got := r.pub.hungup()
	require.Len(t, got, 1)
	require.Equal(t, callID, got[0].CallID)
	require.Equal(t, "USER_BUSY", got[0].Cause)
	require.NotEqual(t, uuid.Nil, got[0].CommandID)

	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Hangups.WithLabelValues("ok")), 0)
}

// TestRouter_HangupErrorPropagates — error path symmetry with Dial.
func TestRouter_HangupErrorPropagates(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	sentinel := errors.New("bridge offline")
	r.pub.hangupErr = sentinel

	err := r.r.Hangup(context.Background(), uuid.New(), "NORMAL_CLEARING")
	require.ErrorIs(t, err, sentinel)
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.Hangups.WithLabelValues("error")), 0)
}

// TestRouter_SubscribeRequiresHandler — a nil handler is rejected at
// the dialer Router boundary so the underlying consumer is not asked
// to subscribe a no-op.
func TestRouter_SubscribeRequiresHandler(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	_, err := r.r.Subscribe(context.Background(), uuid.New(), nil)
	require.Error(t, err)
}

// TestRouter_SubscribeRoutesAnsweredEvent — a telephony EventAnswer
// is translated into a dialer "answered" event and delivered to the
// user handler. The metric records both the receive and (no) drop.
func TestRouter_SubscribeRoutesAnsweredEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	tenantID := uuid.New()
	callID := uuid.New()

	var got dialerapi.ChannelEvent
	var hits int
	unsubscribe, err := r.r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, evt dialerapi.ChannelEvent) error {
			hits++
			got = evt
			return nil
		},
	)
	require.NoError(t, err)
	require.NotNil(t, unsubscribe)
	t.Cleanup(unsubscribe)

	require.NoError(t, r.con.Push(context.Background(), telephonyapi.ChannelEvent{
		EventID:  uuid.New(),
		TenantID: tenantID,
		CallID:   callID,
		FSNode:   "fs1.example.com:8021",
		Type:     telephonyapi.EventAnswer,
	}))

	require.Equal(t, 1, hits)
	require.Equal(t, callID, got.CallID)
	require.Equal(t, "answered", got.Type)
	require.Equal(t, "fs1.example.com:8021", got.FsNode)

	require.InDelta(t, 1.0,
		testutil.ToFloat64(r.metrics.EventsReceived.WithLabelValues("answer")), 0)
	require.InDelta(t, 0.0,
		testutil.ToFloat64(r.metrics.EventsDropped.WithLabelValues("answer")), 0)
}

// TestRouter_SubscribeRoutesHangupEvent — hangup carries Cause +
// Duration through to the dialer projection.
func TestRouter_SubscribeRoutesHangupEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	tenantID := uuid.New()
	callID := uuid.New()

	var got dialerapi.ChannelEvent
	unsubscribe, err := r.r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, evt dialerapi.ChannelEvent) error {
			got = evt
			return nil
		},
	)
	require.NoError(t, err)
	t.Cleanup(unsubscribe)

	require.NoError(t, r.con.Push(context.Background(), telephonyapi.ChannelEvent{
		TenantID:    tenantID,
		CallID:      callID,
		FSNode:      "fs2.example.com:8021",
		Type:        telephonyapi.EventHangup,
		HangupCause: "NO_ANSWER",
		DurationMS:  37500,
		Timestamp:   time.Now(),
	}))

	require.Equal(t, "hangup", got.Type)
	require.Equal(t, "NO_ANSWER", got.Cause)
	require.Equal(t, 37500, got.Duration)
	require.Equal(t, "fs2.example.com:8021", got.FsNode)
}

// TestRouter_SubscribeBridgeFoldsToAnswered — the bridge event is the
// "already answered" confirmation; the dialer treats it as a duplicate
// answered event. The handler still fires (so a downstream FSM that
// re-applies "answered" gets a nudge to verify state); production FSM
// implementations idempotent-no-op on already-applied events.
func TestRouter_SubscribeBridgeFoldsToAnswered(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	tenantID := uuid.New()

	var seen []string
	unsubscribe, err := r.r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, evt dialerapi.ChannelEvent) error {
			seen = append(seen, evt.Type)
			return nil
		},
	)
	require.NoError(t, err)
	t.Cleanup(unsubscribe)

	require.NoError(t, r.con.Push(context.Background(), telephonyapi.ChannelEvent{
		TenantID: tenantID,
		CallID:   uuid.New(),
		Type:     telephonyapi.EventBridge,
	}))
	require.Equal(t, []string{"answered"}, seen)
}

// TestRouter_SubscribeDropsDTMF — DTMF events are intentionally
// dropped at the translator. The dialer handler is NOT invoked; the
// EventsDropped metric ticks under the raw "dtmf" label.
func TestRouter_SubscribeDropsDTMF(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	tenantID := uuid.New()

	var hits int
	unsubscribe, err := r.r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, _ dialerapi.ChannelEvent) error {
			hits++
			return nil
		},
	)
	require.NoError(t, err)
	t.Cleanup(unsubscribe)

	require.NoError(t, r.con.Push(context.Background(), telephonyapi.ChannelEvent{
		TenantID: tenantID,
		CallID:   uuid.New(),
		Type:     telephonyapi.EventDTMF,
	}))

	require.Equal(t, 0, hits, "DTMF must NOT reach the dialer handler")
	require.InDelta(t, 1.0,
		testutil.ToFloat64(r.metrics.EventsReceived.WithLabelValues("dtmf")), 0)
	require.InDelta(t, 1.0,
		testutil.ToFloat64(r.metrics.EventsDropped.WithLabelValues("dtmf")), 0)
	require.InDelta(t, 0.0, testutil.ToFloat64(r.metrics.EventsTranslationErrors), 0,
		"intentional drop is NOT a translation error")
}

// TestRouter_SubscribeDropsUnbridgeAndRecordStop — symmetric coverage
// for the other two intentional drops.
func TestRouter_SubscribeDropsUnbridgeAndRecordStop(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	tenantID := uuid.New()

	var hits int
	unsubscribe, err := r.r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, _ dialerapi.ChannelEvent) error {
			hits++
			return nil
		},
	)
	require.NoError(t, err)
	t.Cleanup(unsubscribe)

	for _, et := range []telephonyapi.ChannelEventType{
		telephonyapi.EventUnbridge,
		telephonyapi.EventRecordStop,
	} {
		require.NoError(t, r.con.Push(context.Background(), telephonyapi.ChannelEvent{
			TenantID: tenantID,
			CallID:   uuid.New(),
			Type:     et,
		}))
	}

	require.Equal(t, 0, hits)
	require.InDelta(t, 1.0,
		testutil.ToFloat64(r.metrics.EventsDropped.WithLabelValues("unbridge")), 0)
	require.InDelta(t, 1.0,
		testutil.ToFloat64(r.metrics.EventsDropped.WithLabelValues("record_stop")), 0)
}

// TestRouter_SubscribeUnknownEventTicksTranslationError — a future
// telephony enum addition this package has not been taught about
// drops AND ticks the EventsTranslationErrors counter so ops sees
// a non-zero rate alerting them to update the translator.
func TestRouter_SubscribeUnknownEventTicksTranslationError(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	tenantID := uuid.New()

	var hits int
	unsubscribe, err := r.r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, _ dialerapi.ChannelEvent) error {
			hits++
			return nil
		},
	)
	require.NoError(t, err)
	t.Cleanup(unsubscribe)

	require.NoError(t, r.con.Push(context.Background(), telephonyapi.ChannelEvent{
		TenantID: tenantID,
		CallID:   uuid.New(),
		Type:     telephonyapi.ChannelEventType("future_event"),
	}))

	require.Equal(t, 0, hits)
	require.InDelta(t, 1.0,
		testutil.ToFloat64(r.metrics.EventsDropped.WithLabelValues("future_event")), 0)
	require.InDelta(t, 1.0, testutil.ToFloat64(r.metrics.EventsTranslationErrors), 0)
}

// TestRouter_SubscribeHandlerErrorPropagates — a user-handler error
// flows back through the consumer's Push call so the underlying
// telephony.Consumer can NACK + re-deliver. Swallowing here would
// break the at-least-once guarantee of the bridge.
func TestRouter_SubscribeHandlerErrorPropagates(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	tenantID := uuid.New()

	sentinel := errors.New("dialer FSM said no")
	unsubscribe, err := r.r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, _ dialerapi.ChannelEvent) error {
			return sentinel
		},
	)
	require.NoError(t, err)
	t.Cleanup(unsubscribe)

	err = r.con.Push(context.Background(), telephonyapi.ChannelEvent{
		TenantID: tenantID,
		CallID:   uuid.New(),
		Type:     telephonyapi.EventAnswer,
	})
	require.ErrorIs(t, err, sentinel)
}

// TestRouter_SubscribeForwardsTenantToConsumer — the TenantID we pass
// to dialer.Router.Subscribe lands at the underlying consumer's
// Subscribe call. Ensures the wrapper does not accidentally
// substitute uuid.Nil or capture a closure-stale tenant.
func TestRouter_SubscribeForwardsTenantToConsumer(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	tenantID := uuid.New()

	unsubscribe, err := r.r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, _ dialerapi.ChannelEvent) error { return nil },
	)
	require.NoError(t, err)
	t.Cleanup(unsubscribe)

	r.con.mu.Lock()
	gotTenant := r.con.tenantID
	r.con.mu.Unlock()
	require.Equal(t, tenantID, gotTenant)
}

// TestRouter_SubscribeUnsubscribeIsDelegated — calling the returned
// unsubscribe func ticks the underlying consumer's unsubscribe
// counter. Ensures we do not silently shadow the consumer's
// unsubscribe with a no-op closure.
func TestRouter_SubscribeUnsubscribeIsDelegated(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	tenantID := uuid.New()

	unsubscribe, err := r.r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, _ dialerapi.ChannelEvent) error { return nil },
	)
	require.NoError(t, err)

	unsubscribe()

	r.con.mu.Lock()
	gotUnsub := r.con.unsubscribed
	r.con.mu.Unlock()
	require.Equal(t, 1, gotUnsub)
}

// TestRouter_SubscribeErrorReturnsNilUnsubscribe — when the underlying
// consumer's Subscribe fails the dialer Router returns a nil
// unsubscribe (per the api.Router contract: caller need not nil-check).
func TestRouter_SubscribeErrorReturnsNilUnsubscribe(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.con.subscribeErr = errors.New("nats not connected")

	unsubscribe, err := r.r.Subscribe(context.Background(), uuid.New(),
		func(_ context.Context, _ dialerapi.ChannelEvent) error { return nil },
	)
	require.Error(t, err)
	require.Nil(t, unsubscribe)
}

// TestRouter_NilMetricsTolerated — building a Router without metrics
// is supported; every observe path no-ops. Belt-and-braces against a
// future regression that adds a non-nil-checked metric tick.
func TestRouter_NilMetricsTolerated(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	con := &fakeConsumer{}
	r, err := router.New(router.Config{
		Publisher: pub,
		Consumer:  con,
		Logger:    zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	// Dial / Hangup must not panic with nil metrics.
	require.NoError(t, r.Dial(context.Background(), dialerapi.DialRequest{
		CallID:      uuid.New(),
		TenantID:    uuid.New(),
		OperatorExt: "lst_1",
		Phone:       "+79990001111",
		FsNode:      "fs1.example.com:8021",
	}))
	require.NoError(t, r.Hangup(context.Background(), uuid.New(), "NORMAL_CLEARING"))

	// Subscribe + Push of a dropped event also exercises the
	// metric-nil branches in the wrapper.
	tenantID := uuid.New()
	unsubscribe, err := r.Subscribe(context.Background(), tenantID,
		func(_ context.Context, _ dialerapi.ChannelEvent) error { return nil },
	)
	require.NoError(t, err)
	t.Cleanup(unsubscribe)
	require.NoError(t, con.Push(context.Background(), telephonyapi.ChannelEvent{
		TenantID: tenantID,
		CallID:   uuid.New(),
		Type:     telephonyapi.EventDTMF,
	}))
	// Also drive the unknown-event path to exercise the
	// observeTranslationError nil-tolerated branch.
	require.NoError(t, con.Push(context.Background(), telephonyapi.ChannelEvent{
		TenantID: tenantID,
		CallID:   uuid.New(),
		Type:     telephonyapi.ChannelEventType("future_event"),
	}))
}

// TestRouter_RegisterMetricsNilRegistererPanics — the contract is
// "panic on nil reg" (matches FSM/queue/RDD packages).
func TestRouter_RegisterMetricsNilRegistererPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { router.RegisterMetrics(nil) })
}

// Compile-time interface assertion — fakes satisfy the telephony.api
// surfaces. If the api drifts the test package fails to compile,
// surfacing the breakage at exactly the same checkpoint as the
// production assertion in nats_router.go.
var (
	_ telephonyapi.CommandPublisher = (*fakePublisher)(nil)
	_ telephonyapi.EventConsumer    = (*fakeConsumer)(nil)
)

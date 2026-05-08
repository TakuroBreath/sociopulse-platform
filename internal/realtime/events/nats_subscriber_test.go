// nats_subscriber_test.go — behaviour tests for *NATSSubscriber.
//
// Test discipline:
//
//   - No real NATS in this package's unit tests. The fakeBus in this
//     file implements eventbus.Subscriber and lets each test drive
//     synthetic messages through the registered handler.
//   - The hubBroadcastRecorder implements HubBroadcaster, captures every
//     dispatch tuple and returns a configurable fan-out count so tests
//     can assert on (topic, filter, payload, count) tuples.
//   - Goleak guards against a regression that spawns a stray goroutine
//     in this package — the *NATSSubscriber owns no goroutines
//     directly (the underlying eventbus.Subscriber does).
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/events"
	"github.com/sociopulse/platform/internal/realtime/service"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// Compile-time guarantees the package exposes the expected contracts.
//
// HubBroadcaster — *service.Hub satisfies it (production wiring).
// Subscriber — fakeBus satisfies it (test wiring).
var (
	_ events.HubBroadcaster = (*service.Hub)(nil)
	_ eventbus.Subscriber   = (*fakeBus)(nil)
	_ events.HubBroadcaster = (*hubBroadcastRecorder)(nil)
)

// --- Fakes ------------------------------------------------------------

// fakeBus is an in-memory eventbus.Subscriber. It records each
// Subscribe call (subject, queue, handler) and lets the test fire
// synthetic messages through Fire.
type fakeBus struct {
	mu            sync.Mutex
	subscriptions []fakeSubscription
	subscribeErr  error
}

type fakeSubscription struct {
	subject string
	queue   string
	handler func(subject string, payload []byte) error
}

func (b *fakeBus) Subscribe(_ context.Context, subject, queue string, handler func(subject string, payload []byte) error) error {
	if b.subscribeErr != nil {
		return b.subscribeErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscriptions = append(b.subscriptions, fakeSubscription{
		subject: subject,
		queue:   queue,
		handler: handler,
	})
	return nil
}

// Fire delivers a synthetic message to every handler whose subject
// pattern matches subject. Pattern matching is the NATS wildcard rule
// (* matches one token; > matches the rest). Returns the first
// non-nil handler error so tests can assert no handler ever returns
// non-nil to the bus (which would trigger redelivery).
func (b *fakeBus) Fire(subject string, payload []byte) error {
	b.mu.Lock()
	subs := append([]fakeSubscription(nil), b.subscriptions...)
	b.mu.Unlock()

	for _, s := range subs {
		if !natsSubjectMatch(s.subject, subject) {
			continue
		}
		if err := s.handler(subject, payload); err != nil {
			return err
		}
	}
	return nil
}

// Subjects returns the subject patterns currently registered. Used by
// tests that assert on the exact set of patterns Start subscribes to.
func (b *fakeBus) Subjects() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.subscriptions))
	for i, s := range b.subscriptions {
		out[i] = s.subject
	}
	return out
}

// Queues returns the queue groups currently registered, in subscription
// order. Tests assert all subscriptions share the configured replicaID
// queue.
func (b *fakeBus) Queues() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.subscriptions))
	for i, s := range b.subscriptions {
		out[i] = s.queue
	}
	return out
}

// natsSubjectMatch implements the subset of NATS wildcard matching
// needed by the tests in this file. Only "*" wildcards (single
// token) are used by the dispatcher's subject patterns; we do not
// emulate ">" terminal wildcards because no current pattern uses one.
func natsSubjectMatch(pattern, subject string) bool {
	pp := strings.Split(pattern, ".")
	sp := strings.Split(subject, ".")
	if len(pp) != len(sp) {
		return false
	}
	for i := range pp {
		if pp[i] == "*" {
			continue
		}
		if pp[i] != sp[i] {
			return false
		}
	}
	return true
}

// hubBroadcastRecorder captures Hub.Broadcast invocations.
type hubBroadcastRecorder struct {
	mu        sync.Mutex
	calls     []recordedBroadcast
	returnVal int
}

type recordedBroadcast struct {
	topic   rtapi.Topic
	payload json.RawMessage
	filter  rtapi.BroadcastFilter
}

func (r *hubBroadcastRecorder) Broadcast(_ context.Context, topic rtapi.Topic, payload json.RawMessage, filter rtapi.BroadcastFilter) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedBroadcast{
		topic:   topic,
		payload: payload,
		filter:  filter,
	})
	return r.returnVal
}

func (r *hubBroadcastRecorder) Calls() []recordedBroadcast {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedBroadcast, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *hubBroadcastRecorder) Last() (recordedBroadcast, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return recordedBroadcast{}, false
	}
	return r.calls[len(r.calls)-1], true
}

// --- Tests ------------------------------------------------------------

func TestNATSSubscriber_Start_RegistersSupportedPatterns(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	hub := &hubBroadcastRecorder{}
	s := events.NewNATSSubscriber(bus, hub, zap.NewNop(), nil, events.WithReplicaID("replica-x"))

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	subjects := bus.Subjects()
	// Five supported patterns; trunks.health intentionally not wired.
	require.ElementsMatch(t, []string{
		"tenant.*.dialer.op.*.state",
		"tenant.*.dialer.queue",
		"tenant.*.telephony.event.*.*",
		"tenant.*.notify.user.*",
		"tenant.*.force.user.*",
	}, subjects)

	// All subscriptions share the replicaID queue.
	queues := bus.Queues()
	require.Len(t, queues, 5)
	for _, q := range queues {
		require.Equal(t, "realtime-replica-replica-x", q)
	}
}

func TestNATSSubscriber_Start_TwiceErrors(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	hub := &hubBroadcastRecorder{}
	s := events.NewNATSSubscriber(bus, hub, nil, nil)

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	err := s.Start(context.Background())
	require.Error(t, err)
}

func TestNATSSubscriber_Start_PropagatesBusError(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{subscribeErr: errors.New("boom")}
	hub := &hubBroadcastRecorder{}
	s := events.NewNATSSubscriber(bus, hub, nil, nil)

	err := s.Start(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "realtime/events:")
}

func TestNATSSubscriber_Stop_Idempotent(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	hub := &hubBroadcastRecorder{}
	s := events.NewNATSSubscriber(bus, hub, nil, nil)

	require.NoError(t, s.Start(context.Background()))
	require.NoError(t, s.Stop())
	// Second Stop is a no-op.
	require.NoError(t, s.Stop())
}

// Each subject pattern → its expected (topic, filter) projection.
func TestNATSSubscriber_DispatchesOperatorsState(t *testing.T) {
	t.Parallel()

	bus, hub := startSubscriber(t)

	payload := []byte(`{"state":"call"}`)
	require.NoError(t, bus.Fire("tenant.tenant-A.dialer.op.op-1.state", payload))

	last, ok := hub.Last()
	require.True(t, ok)
	require.Equal(t, rtapi.TopicOperatorsState, last.topic)
	require.Equal(t, "tenant-A", last.filter.TenantID)
	require.JSONEq(t, string(payload), string(last.payload))
}

func TestNATSSubscriber_DispatchesDialerQueue(t *testing.T) {
	t.Parallel()

	bus, hub := startSubscriber(t)

	payload := []byte(`{"depth":42}`)
	require.NoError(t, bus.Fire("tenant.tenant-B.dialer.queue", payload))

	last, ok := hub.Last()
	require.True(t, ok)
	require.Equal(t, rtapi.TopicDialerQueue, last.topic)
	require.Equal(t, rtapi.BroadcastFilter{TenantID: "tenant-B"}, last.filter)
}

func TestNATSSubscriber_DispatchesCallEvents(t *testing.T) {
	t.Parallel()

	bus, hub := startSubscriber(t)

	payload := []byte(`{"phase":"answered"}`)
	require.NoError(t, bus.Fire("tenant.tenant-C.telephony.event.call-7.bridged", payload))

	last, ok := hub.Last()
	require.True(t, ok)
	require.Equal(t, rtapi.TopicCallEvents, last.topic)
	require.Equal(t, "tenant-C", last.filter.TenantID)
	require.Equal(t, "call-7", last.filter.CallID)
}

func TestNATSSubscriber_DispatchesNotifications(t *testing.T) {
	t.Parallel()

	bus, hub := startSubscriber(t)

	payload := []byte(`{"kind":"info"}`)
	require.NoError(t, bus.Fire("tenant.tenant-D.notify.user.user-9", payload))

	last, ok := hub.Last()
	require.True(t, ok)
	require.Equal(t, rtapi.TopicNotifications, last.topic)
	require.Equal(t, "tenant-D", last.filter.TenantID)
	require.Equal(t, "user-9", last.filter.UserID)
}

func TestNATSSubscriber_DispatchesForceCommands(t *testing.T) {
	t.Parallel()

	bus, hub := startSubscriber(t)

	payload := []byte(`{"cmd":"pause"}`)
	require.NoError(t, bus.Fire("tenant.tenant-E.force.user.user-1", payload))

	last, ok := hub.Last()
	require.True(t, ok)
	require.Equal(t, rtapi.TopicForceCommands, last.topic)
	require.Equal(t, "tenant-E", last.filter.TenantID)
	require.Equal(t, "user-1", last.filter.UserID)
}

func TestNATSSubscriber_MalformedSubject_NoBroadcast(t *testing.T) {
	t.Parallel()

	bus, hub := startSubscriber(t)

	// 6-token operators-state subject with an empty operator slot.
	// Wrong token count for the dialer.queue pattern: too few tokens.
	// Inject directly via a malformed handler invocation. We achieve
	// this by Fire-ing a subject that matches a registered pattern but
	// has an unexpected token count after split — for the dialer.op
	// path, register a synthetic handler call. Simulate with a Fire
	// against the pattern's wildcard token slot collapsed.
	//
	// Concretely: a "tenant.A.dialer.op.state" subject is missing the
	// operator-id token (5 tokens vs the expected 6). Since the
	// subscriber pattern is "tenant.*.dialer.op.*.state" (6 tokens),
	// natsSubjectMatch in fakeBus would not match, so Fire would not
	// invoke the handler at all — that is itself a valid test outcome:
	// a malformed subject simply never reaches the dispatcher. But the
	// dispatcher MUST also be defensive in case the bus delivers a
	// subject under one of the registered patterns whose token count
	// post-split is unexpected (e.g. a future broker bug).
	//
	// We exercise the defensive branch by directly invoking the
	// handler stored on the fakeBus with a wrong-arity subject.

	bus.mu.Lock()
	require.NotEmpty(t, bus.subscriptions, "Start must have registered handlers")
	handler := bus.subscriptions[0].handler // operators.state handler
	bus.mu.Unlock()

	// Operator state pattern expects 6 tokens; supply 5.
	err := handler("tenant.A.dialer.op.state", []byte(`{}`))
	require.NoError(t, err, "handler must always ack — never propagate to bus")

	require.Empty(t, hub.Calls(), "malformed subject must not reach the Hub")
}

func TestNATSSubscriber_EmptyTenant_NoBroadcast(t *testing.T) {
	t.Parallel()

	bus, hub := startSubscriber(t)

	// Subject matches operators.state pattern by token count (6) but
	// the tenant token is empty. Hub.Broadcast would refuse anyway,
	// but the dispatcher must skip earlier (and emit the
	// empty_tenant counter).
	bus.mu.Lock()
	handler := bus.subscriptions[0].handler
	bus.mu.Unlock()

	err := handler("tenant..dialer.op.op-1.state", []byte(`{}`))
	require.NoError(t, err)
	require.Empty(t, hub.Calls())
}

func TestNATSSubscriber_HandlerNeverPropagatesError(t *testing.T) {
	t.Parallel()

	bus, _ := startSubscriber(t)

	bus.mu.Lock()
	handlers := append([]fakeSubscription(nil), bus.subscriptions...)
	bus.mu.Unlock()

	// Fire malformed and well-formed messages through every handler;
	// every invocation must return nil.
	cases := []struct{ subject string }{
		{"bogus"},
		{"tenant..dialer.queue"},
		{"tenant.X.dialer.queue"},
	}
	for _, h := range handlers {
		for _, c := range cases {
			require.NoError(t, h.handler(c.subject, []byte(`{}`)),
				"handler must never return non-nil to the bus")
		}
	}
}

func TestNATSSubscriber_DefaultReplicaID_IsNonEmpty(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	hub := &hubBroadcastRecorder{}
	// No WithReplicaID — constructor must default to a uuid.
	s := events.NewNATSSubscriber(bus, hub, nil, nil)
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	queues := bus.Queues()
	require.NotEmpty(t, queues)
	for _, q := range queues {
		require.NotEqual(t, "realtime-replica-", q,
			"empty replicaID would produce realtime-replica- (no suffix)")
		require.Contains(t, q, "realtime-replica-")
	}
}

func TestNATSSubscriber_ConstructorPanicsOnNilBus(t *testing.T) {
	t.Parallel()

	defer func() {
		require.NotNil(t, recover(), "nil bus must panic at construction")
	}()
	hub := &hubBroadcastRecorder{}
	_ = events.NewNATSSubscriber(nil, hub, nil, nil)
}

func TestNATSSubscriber_ConstructorPanicsOnNilHub(t *testing.T) {
	t.Parallel()

	defer func() {
		require.NotNil(t, recover(), "nil hub must panic at construction")
	}()
	bus := &fakeBus{}
	_ = events.NewNATSSubscriber(bus, nil, nil, nil)
}

func TestNATSSubscriber_EmptyTenant_AcrossAllPatterns(t *testing.T) {
	t.Parallel()

	bus, hub := startSubscriber(t)

	// One synthetic empty-tenant subject per pattern. Every dispatcher
	// extract() must return ok=false → no Hub.Broadcast.
	bus.mu.Lock()
	subs := append([]fakeSubscription(nil), bus.subscriptions...)
	bus.mu.Unlock()

	cases := map[string]string{
		"tenant.*.dialer.op.*.state":   "tenant..dialer.op.op-1.state",
		"tenant.*.dialer.queue":        "tenant..dialer.queue",
		"tenant.*.telephony.event.*.*": "tenant..telephony.event.call-1.bridged",
		"tenant.*.notify.user.*":       "tenant..notify.user.user-1",
		"tenant.*.force.user.*":        "tenant..force.user.user-1",
	}

	for _, sub := range subs {
		empty, ok := cases[sub.subject]
		require.True(t, ok, "test missing empty-tenant case for %q", sub.subject)
		require.NoError(t, sub.handler(empty, []byte(`{}`)))
	}

	require.Empty(t, hub.Calls(), "every empty-tenant subject must skip the Hub")
}

func TestRegisterMetrics_NilRegistererPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		require.NotNil(t, recover(), "nil registerer must panic")
	}()
	_ = events.RegisterMetrics(nil)
}

func TestNATSSubscriber_NilLogger_DefaultsToNop(t *testing.T) {
	t.Parallel()

	bus := &fakeBus{}
	hub := &hubBroadcastRecorder{}
	s := events.NewNATSSubscriber(bus, hub, nil, nil)
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	// A subsequent Fire must not panic on the nil logger path.
	require.NoError(t, bus.Fire("tenant.X.dialer.queue", []byte(`{}`)))
	require.Len(t, hub.Calls(), 1)
}

func TestNATSSubscriber_FanoutHistogramObserved(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := events.RegisterMetrics(reg)

	bus := &fakeBus{}
	hub := &hubBroadcastRecorder{returnVal: 7}
	s := events.NewNATSSubscriber(bus, hub, nil, metrics)
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	require.NoError(t, bus.Fire("tenant.X.dialer.queue", []byte(`{}`)))

	require.InDelta(t, 1.0, counterValue(t, reg,
		"realtime_dispatcher_messages_total", map[string]string{"topic": "dialer.queue"}), 0.001)

	// Histogram observed exactly one sample with value 7.
	count, sum := histogramSnapshot(t, reg, "realtime_dispatcher_fanout_size")
	require.Equal(t, uint64(1), count)
	require.InDelta(t, 7.0, sum, 0.001)
}

func TestNATSSubscriber_MalformedAndEmptyTenantCounters(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := events.RegisterMetrics(reg)

	bus := &fakeBus{}
	hub := &hubBroadcastRecorder{}
	s := events.NewNATSSubscriber(bus, hub, nil, metrics)
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	bus.mu.Lock()
	opHandler := bus.subscriptions[0].handler
	bus.mu.Unlock()

	require.NoError(t, opHandler("tenant.A.dialer.op.state", []byte(`{}`)))     // malformed
	require.NoError(t, opHandler("tenant..dialer.op.op-1.state", []byte(`{}`))) // empty tenant

	require.InDelta(t, 1.0, counterValue(t, reg,
		"realtime_dispatcher_dispatch_failures_total",
		map[string]string{"topic": "operators.state", "reason": "malformed_subject"}), 0.001)
	require.InDelta(t, 1.0, counterValue(t, reg,
		"realtime_dispatcher_dispatch_failures_total",
		map[string]string{"topic": "operators.state", "reason": "empty_tenant"}), 0.001)
}

func TestNATSSubscriber_ConcurrentDispatchRaceClean(t *testing.T) {
	t.Parallel()

	bus, hub := startSubscriber(t)

	const goroutines = 8
	const messagesEach = 50

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range messagesEach {
				require.NoError(t, bus.Fire("tenant.X.dialer.queue", []byte(`{}`)))
			}
		})
	}
	wg.Wait()

	require.Len(t, hub.Calls(), goroutines*messagesEach)
}

// --- helpers ----------------------------------------------------------

func startSubscriber(t *testing.T) (*fakeBus, *hubBroadcastRecorder) {
	t.Helper()
	bus := &fakeBus{}
	hub := &hubBroadcastRecorder{}
	s := events.NewNATSSubscriber(bus, hub, zap.NewNop(), nil, events.WithReplicaID("replica-x"))
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })
	return bus, hub
}

func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m.GetLabel(), labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func histogramSnapshot(t *testing.T, reg *prometheus.Registry, name string) (uint64, float64) {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var count uint64
		var sum float64
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			count += h.GetSampleCount()
			sum += h.GetSampleSum()
		}
		return count, sum
	}
	return 0, 0
}

func labelsMatch(actual []*dto.LabelPair, want map[string]string) bool {
	if len(actual) != len(want) {
		return false
	}
	for _, lp := range actual {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

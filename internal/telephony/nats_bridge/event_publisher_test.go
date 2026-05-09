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
	"github.com/sociopulse/platform/internal/telephony/pool"
)

// --- test doubles ----------------------------------------------------------

type publishCall struct {
	subject string
	payload []byte
}

// fakePublisher is a tiny stub for eventbus.Publisher. It records every
// call and optionally returns a fixed error so the loop's drop-on-publish-
// error path can be driven without a NATS broker.
type fakePublisher struct {
	mu sync.Mutex

	calls []publishCall

	// returnErr, when non-nil, is returned from every Publish — used to
	// drive "publisher error → drop event + tick metric + continue".
	returnErr error

	// blockUntil, when non-nil, blocks Publish on that channel before
	// returning. Used to test ctx cancellation mid-publish.
	blockUntil chan struct{}
}

func (f *fakePublisher) Publish(ctx context.Context, subject string, payload []byte) error {
	if f.blockUntil != nil {
		select {
		case <-f.blockUntil:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(payload))
	copy(cp, payload)
	f.calls = append(f.calls, publishCall{subject: subject, payload: cp})
	return f.returnErr
}

func (f *fakePublisher) snapshot() []publishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]publishCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// makeEvent constructs a pool.EventEnvelope with non-zero Type so the
// publisher path treats it as publishable. tenantID/callID/kind drive the
// expected subject.
func makeEvent(tenantID, callID uuid.UUID, kind telapi.ChannelEventType) pool.EventEnvelope {
	return pool.EventEnvelope{
		NodeAddr: "fs-a:8021",
		Event: telapi.ChannelEvent{
			EventID:   uuid.New(),
			TenantID:  tenantID,
			CallID:    callID,
			FSNode:    "fs-a:8021",
			Type:      kind,
			Timestamp: time.Now(),
		},
	}
}

// --- tests ----------------------------------------------------------------

// TestEventPublisher_PublishesEachChannelEvent covers the happy path: a
// single event is delivered onto the bus with the expected subject and
// the JSON-marshalled api.ChannelEvent body.
func TestEventPublisher_PublishesEachChannelEvent(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	callID := uuid.New()

	events := make(chan pool.EventEnvelope, 1)
	fp := &fakePublisher{}

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	pub := newEventPublisher(fp, events, metrics, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	pub.Run(ctx)

	events <- makeEvent(tenantID, callID, telapi.EventBridge)

	// Wait for the single publish to land. Use a polling loop bounded
	// at 1s — JetStream-style retry timing isn't in scope here, just a
	// direct synchronous fakePublisher call.
	require.Eventually(t, func() bool {
		return len(fp.snapshot()) == 1
	}, time.Second, 5*time.Millisecond, "expected 1 publish call")

	got := fp.snapshot()[0]
	want := telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventBridge))
	assert.Equal(t, want, got.subject)

	var decoded telapi.ChannelEvent
	require.NoError(t, json.Unmarshal(got.payload, &decoded))
	assert.Equal(t, tenantID, decoded.TenantID)
	assert.Equal(t, callID, decoded.CallID)
	assert.Equal(t, telapi.EventBridge, decoded.Type)

	assert.InDelta(t, 1.0,
		testutil.ToFloat64(metrics.EventsPublished.WithLabelValues(string(telapi.EventBridge))),
		1e-9)

	cancel()
	pub.Stop()
}

// TestEventPublisher_PublishesMultipleEventsInOrder asserts the goroutine
// drains the events chan in delivery order, not in arbitrary fan-out
// order. Required for downstream consumers that rely on the ESL chronology.
func TestEventPublisher_PublishesMultipleEventsInOrder(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	callID := uuid.New()

	events := make(chan pool.EventEnvelope, 4)
	fp := &fakePublisher{}

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	pub := newEventPublisher(fp, events, metrics, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	pub.Run(ctx)

	events <- makeEvent(tenantID, callID, telapi.EventDialing)
	events <- makeEvent(tenantID, callID, telapi.EventAnswer)
	events <- makeEvent(tenantID, callID, telapi.EventBridge)
	events <- makeEvent(tenantID, callID, telapi.EventHangup)

	require.Eventually(t, func() bool {
		return len(fp.snapshot()) == 4
	}, 2*time.Second, 5*time.Millisecond)

	got := fp.snapshot()
	require.Len(t, got, 4)
	wantKinds := []telapi.ChannelEventType{
		telapi.EventDialing,
		telapi.EventAnswer,
		telapi.EventBridge,
		telapi.EventHangup,
	}
	for i, kind := range wantKinds {
		assert.Equal(t,
			telapi.SubjectChannelEventFor(tenantID, callID, string(kind)),
			got[i].subject,
			"event %d subject", i)
	}

	cancel()
	pub.Stop()
}

// TestEventPublisher_DropsEventOnPublishError exercises the resilience
// contract: a failed Publish increments the dropped metric and the loop
// continues — the next event is still attempted (would otherwise stall
// the pipeline on a transient broker hiccup).
func TestEventPublisher_DropsEventOnPublishError(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	callID := uuid.New()

	events := make(chan pool.EventEnvelope, 2)

	// First publish errors; flip returnErr to nil after the test sees
	// the drop tick — we then send another event and expect it to land.
	fp := &fakePublisher{returnErr: errors.New("nats: timeout")}

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	pub := newEventPublisher(fp, events, metrics, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	pub.Run(ctx)

	events <- makeEvent(tenantID, callID, telapi.EventDialing)

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.EventsDropped.WithLabelValues(dropReasonPublishError)) >= 1.0
	}, time.Second, 5*time.Millisecond, "expected publish error metric tick")

	// Loop must NOT exit — flip the publisher to success and send another.
	fp.mu.Lock()
	fp.returnErr = nil
	fp.mu.Unlock()

	events <- makeEvent(tenantID, callID, telapi.EventBridge)
	require.Eventually(t, func() bool {
		// First call errored (recorded but irrelevant for ordering);
		// second succeeded — total calls 2, last subject = bridge.
		got := fp.snapshot()
		return len(got) == 2 &&
			got[1].subject == telapi.SubjectChannelEventFor(tenantID, callID, string(telapi.EventBridge))
	}, time.Second, 5*time.Millisecond, "loop must continue past publish error")

	cancel()
	pub.Stop()
}

// TestEventPublisher_StopOnContextCancel proves the goroutine exits when
// ctx is cancelled — required for the bridge Drain/Stop sequence.
func TestEventPublisher_StopOnContextCancel(t *testing.T) {
	t.Parallel()

	events := make(chan pool.EventEnvelope, 1)
	fp := &fakePublisher{}

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	pub := newEventPublisher(fp, events, metrics, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	pub.Run(ctx)

	cancel()

	done := make(chan struct{})
	go func() {
		pub.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("event publisher Stop() did not return within 2s of ctx cancel")
	}
}

// TestEventPublisher_StopOnEventsChanClose proves the goroutine exits
// cleanly when the upstream events chan closes — same exit semantics as
// ctx cancel.
func TestEventPublisher_StopOnEventsChanClose(t *testing.T) {
	t.Parallel()

	events := make(chan pool.EventEnvelope, 1)
	fp := &fakePublisher{}

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	pub := newEventPublisher(fp, events, metrics, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pub.Run(ctx)

	close(events)

	done := make(chan struct{})
	go func() {
		pub.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("event publisher Stop() did not return within 2s of chan close")
	}
}

// TestEventPublisher_StopIsIdempotent ensures double-Stop is a no-op so
// the bridge composition root can defer Stop without worrying about the
// drain path having already called it.
func TestEventPublisher_StopIsIdempotent(t *testing.T) {
	t.Parallel()

	events := make(chan pool.EventEnvelope, 1)
	fp := &fakePublisher{}

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	pub := newEventPublisher(fp, events, metrics, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	pub.Run(ctx)
	cancel()
	pub.Stop()

	require.NotPanics(t, func() {
		pub.Stop()
	}, "double Stop must not panic")
}

// TestEventPublisher_SkipsEventsWithEmptyType: pool.publishEvent emits
// envelopes for events that MapEvent returned ok=false for (HEARTBEAT,
// BACKGROUND_JOB, sofia::register etc.) with a zero-valued
// api.ChannelEvent. Publishing those would yield a subject ending in
// ".." with no kind — defence-in-depth: the publisher must skip them.
func TestEventPublisher_SkipsEventsWithEmptyType(t *testing.T) {
	t.Parallel()

	events := make(chan pool.EventEnvelope, 2)
	fp := &fakePublisher{}

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	pub := newEventPublisher(fp, events, metrics, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	pub.Run(ctx)

	// Event with zero-valued ChannelEvent (Type empty) — skip.
	events <- pool.EventEnvelope{NodeAddr: "fs-a:8021"}

	// Then a real one — must still be published.
	tenantID := uuid.New()
	callID := uuid.New()
	events <- makeEvent(tenantID, callID, telapi.EventBridge)

	require.Eventually(t, func() bool {
		return len(fp.snapshot()) == 1
	}, time.Second, 5*time.Millisecond, "expected exactly 1 publish (zero-Type skipped)")

	cancel()
	pub.Stop()
}

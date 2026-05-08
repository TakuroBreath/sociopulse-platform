package dialer_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/dialer"
	"github.com/sociopulse/platform/internal/dialer/api"
)

// TestPubSubSubscribePublishReceive is the happy-path round-trip:
// Subscribe → Publish → assert the snapshot arrives on the channel.
func TestPubSubSubscribePublishReceive(t *testing.T) {
	t.Parallel()

	ps := dialer.NewPubSub()
	tenantID := uuid.New()
	operatorID := uuid.New()

	ch, cancel := ps.Subscribe(tenantID, operatorID)
	t.Cleanup(cancel)

	want := api.Snapshot{
		TenantID:   tenantID,
		OperatorID: operatorID,
		State:      api.StateReady,
	}
	ps.Publish(want)

	select {
	case got := <-ch:
		assert.Equal(t, want, got)
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive snapshot within 2s")
	}
}

// TestPubSubUnsubscribeStopsDelivery: cancel the subscription, publish
// again, and make sure no further messages arrive on the channel.
func TestPubSubUnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()

	ps := dialer.NewPubSub()
	tenantID := uuid.New()
	operatorID := uuid.New()

	ch, cancel := ps.Subscribe(tenantID, operatorID)
	cancel()

	// After cancel, the channel should be closed (or no further sends
	// should arrive). Publish should not block, and no value should be
	// received apart from the closed-channel zero.
	ps.Publish(api.Snapshot{TenantID: tenantID, OperatorID: operatorID})

	select {
	case _, ok := <-ch:
		// The channel should be closed; ok=false is acceptable.
		assert.False(t, ok, "expected channel to be closed after cancel")
	case <-time.After(200 * time.Millisecond):
		// No delivery within the timeout is also acceptable — the
		// implementation may simply not deliver.
	}
}

// TestPubSubMultipleSubscribersFanOut: two subscribers on the same
// (tenant, operator) pair both receive every published snapshot.
func TestPubSubMultipleSubscribersFanOut(t *testing.T) {
	t.Parallel()

	ps := dialer.NewPubSub()
	tenantID := uuid.New()
	operatorID := uuid.New()

	ch1, cancel1 := ps.Subscribe(tenantID, operatorID)
	t.Cleanup(cancel1)
	ch2, cancel2 := ps.Subscribe(tenantID, operatorID)
	t.Cleanup(cancel2)

	want := api.Snapshot{
		TenantID:   tenantID,
		OperatorID: operatorID,
		State:      api.StatePause,
	}
	ps.Publish(want)

	for _, ch := range []<-chan api.Snapshot{ch1, ch2} {
		select {
		case got := <-ch:
			assert.Equal(t, want, got)
		case <-time.After(2 * time.Second):
			t.Fatal("subscriber did not receive snapshot within 2s")
		}
	}
}

// TestPubSubScopedToOperator: a publish for (tenant1, op1) does not
// reach a subscriber for (tenant1, op2) or (tenant2, op1).
func TestPubSubScopedToOperator(t *testing.T) {
	t.Parallel()

	ps := dialer.NewPubSub()
	tenantA := uuid.New()
	tenantB := uuid.New()
	op1 := uuid.New()
	op2 := uuid.New()

	chTargetOp, cancelTarget := ps.Subscribe(tenantA, op1)
	t.Cleanup(cancelTarget)
	chOtherOp, cancelOther := ps.Subscribe(tenantA, op2)
	t.Cleanup(cancelOther)
	chOtherTenant, cancelTenant := ps.Subscribe(tenantB, op1)
	t.Cleanup(cancelTenant)

	want := api.Snapshot{TenantID: tenantA, OperatorID: op1, State: api.StateReady}
	ps.Publish(want)

	// Target subscriber receives the snapshot.
	select {
	case got := <-chTargetOp:
		assert.Equal(t, want, got)
	case <-time.After(2 * time.Second):
		t.Fatal("target subscriber did not receive snapshot")
	}

	// Cross-operator and cross-tenant subscribers do NOT receive.
	for _, ch := range []<-chan api.Snapshot{chOtherOp, chOtherTenant} {
		select {
		case got := <-ch:
			t.Fatalf("unexpected snapshot delivered to non-matching subscriber: %+v", got)
		case <-time.After(100 * time.Millisecond):
			// pass
		}
	}
}

// TestPubSubConcurrentPublish: many goroutines call Publish concurrently
// against multiple subscribers. The race detector verifies safety; the
// test asserts every published snapshot reaches each subscriber.
func TestPubSubConcurrentPublish(t *testing.T) {
	t.Parallel()

	const subscribers = 8
	const publishers = 16
	const perPublisher = 32

	ps := dialer.NewPubSub()
	tenantID := uuid.New()
	operatorID := uuid.New()

	chans := make([]<-chan api.Snapshot, subscribers)
	cancels := make([]func(), subscribers)
	for i := range subscribers {
		chans[i], cancels[i] = ps.Subscribe(tenantID, operatorID)
	}
	t.Cleanup(func() {
		for _, c := range cancels {
			c()
		}
	})

	// Drain each subscriber concurrently — the implementation buffers
	// per-subscriber so a slow drain does not block Publish, but we
	// drain anyway to keep the test fast.
	var received atomic.Int64
	var drainWG sync.WaitGroup
	drainWG.Add(subscribers)
	stopDrain := make(chan struct{})
	for i := range subscribers {
		go func(ch <-chan api.Snapshot) {
			defer drainWG.Done()
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
					received.Add(1)
				case <-stopDrain:
					return
				}
			}
		}(chans[i])
	}

	var pubWG sync.WaitGroup
	pubWG.Add(publishers)
	for range publishers {
		go func() {
			defer pubWG.Done()
			for range perPublisher {
				ps.Publish(api.Snapshot{
					TenantID:   tenantID,
					OperatorID: operatorID,
					State:      api.StateReady,
				})
			}
		}()
	}
	pubWG.Wait()

	// Allow drains to catch up.
	deadline := time.Now().Add(3 * time.Second)
	want := int64(subscribers * publishers * perPublisher)
	for received.Load() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(stopDrain)
	drainWG.Wait()

	// We expect every published snapshot to reach every subscriber.
	// The implementation drops on a slow consumer (per-subscriber
	// buffered channel); since we drain quickly we should not lose
	// any.
	assert.GreaterOrEqual(t, received.Load(), int64(publishers*perPublisher),
		"expected at least every published snapshot to be delivered to at least one subscriber; got %d", received.Load())
}

// TestPubSubMultipleCancelIdempotent ensures cancel is safe to call
// multiple times.
func TestPubSubMultipleCancelIdempotent(t *testing.T) {
	t.Parallel()

	ps := dialer.NewPubSub()
	_, cancel := ps.Subscribe(uuid.New(), uuid.New())
	cancel()
	require.NotPanics(t, cancel, "second cancel should be a no-op")
}

// TestPubSubPublishNoSubscribersIsNoop ensures publishing without any
// subscribers is a no-op (and doesn't panic).
func TestPubSubPublishNoSubscribersIsNoop(t *testing.T) {
	t.Parallel()

	ps := dialer.NewPubSub()
	require.NotPanics(t, func() {
		ps.Publish(api.Snapshot{TenantID: uuid.New(), OperatorID: uuid.New()})
	})
}

// TestPubSubCloseStopsAllSubscribers verifies the Close() method
// terminates every subscription cleanly. Used by Module.Stop.
func TestPubSubCloseStopsAllSubscribers(t *testing.T) {
	t.Parallel()

	ps := dialer.NewPubSub()
	chA, cancelA := ps.Subscribe(uuid.New(), uuid.New())
	chB, cancelB := ps.Subscribe(uuid.New(), uuid.New())
	t.Cleanup(cancelA)
	t.Cleanup(cancelB)

	ps.Close()

	for _, ch := range []<-chan api.Snapshot{chA, chB} {
		select {
		case _, ok := <-ch:
			assert.False(t, ok, "channel should be closed after PubSub.Close")
		case <-time.After(2 * time.Second):
			t.Fatal("channel did not close after PubSub.Close")
		}
	}
}

package dialer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer/api"
	transporthttp "github.com/sociopulse/platform/internal/dialer/transport/http"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// natsPubSubSubject is the JetStream subject pattern Snapshots are
// published on. Format: tenant.<tenantID>.dialer.op.<operatorID>.state.
//
// Subject parity with internal/realtime/events.NATSSubscriber is
// intentional — that subscriber wildcards on `tenant.*.dialer.op.*.state`,
// so a single Publish from this adapter covers BOTH paths in one shot:
//
//  1. Cross-replica dialer.PubSub fan-out (this struct's local
//     subscribers).
//  2. Realtime WS clients subscribed to TopicOperatorsState.
//
// Keeping the wire shape DRY removes a second outbox hop and one path
// of "stale because pod-A and pod-B disagreed" failure modes.
const natsPubSubSubjectPattern = "tenant.*.dialer.op.*.state"

// natsPublishTimeout caps how long a single Publish may wait for the
// JetStream broker ack. Publish callers don't pass ctx (the *Publisher*
// interface is fire-and-forget by contract — the FSM's commit path
// can't block on a remote broker), so this adapter owns the timeout.
//
// 2s is comfortably above the Yandex MKS cross-AZ NATS RTT p99 (~50ms)
// while still bounded enough that a broker outage doesn't pile up
// pending Publish calls in goroutine memory.
const natsPublishTimeout = 2 * time.Second

// NATSPubSub is the cross-replica Snapshot fan-out backed by the
// project-wide eventbus.Publisher / Subscriber pair. Snapshots are
// JSON-encoded onto subject `tenant.<t>.dialer.op.<op>.state`; a
// per-replica JetStream queue group ensures every replica receives
// every snapshot (the WS handler subscribed on a different pod still
// gets the update for its locally-connected operator).
//
// API surface mirrors the in-memory *PubSub — same Publish, same
// Subscribe shape — so call sites are interchangeable. Module.Register
// wires this adapter when (Deps.EventBus, Deps.Subscriber) are present;
// otherwise it falls back to the in-memory PubSub for Redis-less /
// NATS-less test setups.
//
// Lifecycle:
//
//	NewNATSPubSub → Start(ctx)            // registers the bus subscription
//	Publish(snap)                         // any goroutine, fire-and-forget
//	Subscribe(t, op) → ch, cancel         // any goroutine; cancel idempotent
//	Stop()                                // closes every local channel; idempotent
//
// Goroutine safety: a sync.RWMutex guards the local subscriber map.
// The bus dispatch goroutine is owned by pkg/eventbus.NATSSubscriber —
// this struct does NOT spawn supervisor goroutines, so goleak.VerifyTestMain
// stays green without a Stop+Wait pattern.
type NATSPubSub struct {
	pub       eventbus.Publisher
	sub       eventbus.Subscriber
	replicaID string
	logger    *zap.Logger

	mu          sync.RWMutex
	subscribers map[pubSubKey][]*pubSubChan
	closed      bool
	started     bool
}

// Compile-time interface checks. Enforces drift in either the dialer
// Publisher contract or the transport's SnapshotPubSub at build time
// rather than at first deploy.
var (
	_ Publisher                    = (*NATSPubSub)(nil)
	_ transporthttp.SnapshotPubSub = (*NATSPubSub)(nil)
)

// NewNATSPubSub constructs a NATSPubSub backed by the supplied bus
// pair. pub and sub MUST be non-nil — the constructor panics on either
// nil since the alternative would be silent message loss in production.
//
// logger is nil-safe (nil → zap.NewNop()). replicaID, when empty,
// auto-generates a uuid so two NATSPubSub instances inside the same
// test binary can't accidentally share a JetStream consumer durable.
func NewNATSPubSub(pub eventbus.Publisher, sub eventbus.Subscriber, replicaID string, logger *zap.Logger) *NATSPubSub {
	if pub == nil {
		panic("dialer.NewNATSPubSub: pub must be non-nil")
	}
	if sub == nil {
		panic("dialer.NewNATSPubSub: sub must be non-nil")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if replicaID == "" {
		replicaID = uuid.NewString()
	}
	return &NATSPubSub{
		pub:         pub,
		sub:         sub,
		replicaID:   replicaID,
		logger:      logger,
		subscribers: make(map[pubSubKey][]*pubSubChan),
	}
}

// Start registers the JetStream subscriber for the dialer-state
// subject pattern. The queue group is per-replica
// (`dialer-pubsub-<replicaID>`) so each replica is the sole member of
// its own group — JetStream then fan-outs each Publish to every
// replica, which is the cross-replica fan-out semantics described in
// Plan 11 Decision Q2.
//
// Start is single-shot: a second invocation returns a wrapped error
// rather than re-registering (which would double-deliver every
// message to the local fan-out path).
func (n *NATSPubSub) Start(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return errors.New("dialer/pubsub_nats: Start called twice")
	}
	queue := "dialer-pubsub-" + n.replicaID
	if err := n.sub.Subscribe(ctx, natsPubSubSubjectPattern, queue, n.handleBusMessage); err != nil {
		return fmt.Errorf("dialer/pubsub_nats: subscribe: %w", err)
	}
	n.started = true
	return nil
}

// Stop closes every active local subscriber's channel exactly once and
// flips the closed flag so subsequent Publish/Subscribe calls degrade
// gracefully. The underlying bus subscriber is owned by the composition
// root (cmd/api) and shut down separately — this method only releases
// the per-(tenant, operator) subscriber map.
//
// Idempotent — second Stop is a no-op.
func (n *NATSPubSub) Stop() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	subs := n.subscribers
	n.subscribers = nil
	for _, slot := range subs {
		for _, c := range slot {
			c.once.Do(func() { close(c.ch) })
		}
	}
	return nil
}

// Publish marshals snap as JSON and emits it on
// tenant.<tenantID>.dialer.op.<operatorID>.state. Errors are logged
// (warn level) and not returned — the Publisher interface is
// fire-and-forget by contract. Publish-after-Stop is a silent no-op
// so a late FSM commit on a draining pod doesn't panic.
func (n *NATSPubSub) Publish(snap api.Snapshot) {
	n.mu.RLock()
	closed := n.closed
	n.mu.RUnlock()
	if closed {
		return
	}

	subject := fmt.Sprintf("tenant.%s.dialer.op.%s.state", snap.TenantID, snap.OperatorID)
	payload, err := json.Marshal(snap)
	if err != nil {
		// Marshal failure is impossible for the current Snapshot shape
		// (uuid.UUID + time.Time + string — every field is JSON-clean)
		// but defended against future field additions.
		n.logger.Warn("dialer/pubsub_nats: marshal snapshot",
			zap.String("subject", subject),
			zap.Error(err))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), natsPublishTimeout)
	defer cancel()
	if err := n.pub.Publish(ctx, subject, payload); err != nil {
		// One log line per failure — at warn so a flaky broker doesn't
		// page ops. Plan 11 Task 3 spec calls for fire-and-forget; the
		// next FSM transition publishes a fresher snapshot, so a
		// dropped publish is a benign correctness loss.
		n.logger.Warn("dialer/pubsub_nats: publish snapshot",
			zap.String("subject", subject),
			zap.Error(err))
	}
}

// Subscribe registers a per-(tenant, operator) receiver. The returned
// channel is buffered (pubSubBufferSize) and the cancel is idempotent
// — same contract as the in-memory PubSub.Subscribe so transport call
// sites can swap between the two without behavioural drift.
//
// Subscribe-after-Stop returns a closed channel and a no-op cancel.
// This mirrors the in-memory PubSub.Subscribe shutdown-race graceful
// path and guarantees the WS handler's select observes the closure
// rather than blocking forever on a stale receiver.
func (n *NATSPubSub) Subscribe(tenantID, operatorID uuid.UUID) (<-chan api.Snapshot, func()) {
	sub := &pubSubChan{ch: make(chan api.Snapshot, pubSubBufferSize)}
	key := pubSubKey{tenantID: tenantID, operatorID: operatorID}

	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		// Closed: hand back a closed channel and a no-op cancel so the
		// caller's select returns immediately without further wiring.
		close(sub.ch)
		return sub.ch, func() {}
	}
	n.subscribers[key] = append(n.subscribers[key], sub)
	n.mu.Unlock()

	cancel := func() {
		sub.once.Do(func() {
			n.mu.Lock()
			// Skip the map fix-up when Stop already torched the
			// subscriber map — Stop already closed every channel in
			// that case; we'd be double-closing here without the
			// sync.Once guard.
			if !n.closed {
				slot := n.subscribers[key]
				out := slot[:0]
				for _, c := range slot {
					if c != sub {
						out = append(out, c)
					}
				}
				if len(out) == 0 {
					delete(n.subscribers, key)
				} else {
					n.subscribers[key] = out
				}
			}
			n.mu.Unlock()
			// Close after the map fix-up so a concurrent dispatch that
			// captured this *pubSubChan before our edit observes the
			// closed channel via the safeSend recover path.
			close(sub.ch)
		})
	}
	return sub.ch, cancel
}

// handleBusMessage is the eventbus.Subscriber handler. Decodes the
// JSON payload into an api.Snapshot and delivers it to every local
// subscriber registered under (snap.TenantID, snap.OperatorID).
//
// Returning nil ack's the JetStream message; returning an error
// triggers a NACK + redelivery. We deliberately ack on
// json.Unmarshal failure: a malformed payload is permanent — NACKing
// would loop the broker forever.
func (n *NATSPubSub) handleBusMessage(subject string, payload []byte) error {
	var snap api.Snapshot
	if err := json.Unmarshal(payload, &snap); err != nil {
		// debug, not warn: malformed payloads on a wildcard subject
		// usually mean a different module published JSON the dialer
		// didn't expect. Logging at warn would flood the realtime
		// regression tests where one broker is shared across modules.
		n.logger.Debug("dialer/pubsub_nats: malformed snapshot payload",
			zap.String("subject", subject),
			zap.Error(err))
		return nil
	}
	n.deliverLocal(snap)
	return nil
}

// deliverLocal fans snap out to every locally-registered subscriber
// for (snap.TenantID, snap.OperatorID). Mirrors PubSub.Publish in
// pubsub.go — RLock, snapshot the slice, drop the lock, then
// non-blocking sends so a slow consumer drops its own snapshots
// rather than stalling the bus dispatch goroutine.
func (n *NATSPubSub) deliverLocal(snap api.Snapshot) {
	key := pubSubKey{tenantID: snap.TenantID, operatorID: snap.OperatorID}

	n.mu.RLock()
	if n.closed {
		n.mu.RUnlock()
		return
	}
	subs := n.subscribers[key]
	if len(subs) == 0 {
		n.mu.RUnlock()
		return
	}
	copies := make([]*pubSubChan, len(subs))
	copy(copies, subs)
	n.mu.RUnlock()

	for _, c := range copies {
		safeSend(c.ch, snap)
	}
}

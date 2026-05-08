package dialer

import (
	"sync"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/internal/dialer/api"
	transporthttp "github.com/sociopulse/platform/internal/dialer/transport/http"
)

// pubSubBufferSize is the per-subscriber send-channel buffer. A slow WS
// client gets at most this many backlogged snapshots before Publish
// drops new ones for that subscriber — preventing one stuck operator
// from stalling the FSM commit path. 16 is enough room to absorb a
// typical reconnect window without memory bloat.
const pubSubBufferSize = 16

// Publisher is the small interface the dialer FSM uses to broadcast
// successful transitions. Plan 11 swaps the in-memory implementation
// for a NATS-backed one without touching FSM internals.
type Publisher interface {
	// Publish fans the supplied Snapshot to every subscriber registered
	// for (snap.TenantID, snap.OperatorID). Non-blocking — if a
	// subscriber's buffer is full the snapshot is dropped for that
	// subscriber only. Safe to call from any goroutine.
	Publish(snap api.Snapshot)
}

// pubSubKey scopes a subscription to one (tenant, operator) pair.
// Sized as two uuid.UUIDs (32 bytes) so the map's hash key stays
// monomorphic and lookups stay O(1).
type pubSubKey struct {
	tenantID   uuid.UUID
	operatorID uuid.UUID
}

// PubSub is the in-process per-(tenant, operator) Snapshot fan-out
// used by cmd/api. Every successful FSM transition publishes the new
// Snapshot here; WebSocket handlers subscribe and forward each frame
// to the connected operator UI.
//
// Goroutine safety: a sync.RWMutex guards the subscribers map. Publish
// takes the read lock and uses non-blocking sends so a stuck consumer
// only loses its own snapshots — never the entire broadcast.
//
// Plan 11 will replace this with a NATS-backed cluster fan-out so a
// snapshot published on pod A reaches a WS subscriber on pod B. Until
// then this is single-pod-only — acceptable because cmd/api is the
// only HTTP entry point and an operator's WS connection is tied to
// the same pod that processed their transition.
type PubSub struct {
	mu          sync.RWMutex
	subscribers map[pubSubKey][]*pubSubChan
	closed      bool
}

// pubSubChan wraps a buffered channel + cancel sentinel. The cancel
// closure (returned to the caller) flips done and removes the slot
// from the parent map; flipping done first lets a concurrent Publish
// detect the cancellation between map walks.
type pubSubChan struct {
	ch   chan api.Snapshot
	once sync.Once
}

// Compile-time interface checks. Surfaces drift in either the
// Publisher contract or the transport's SnapshotPubSub the moment it
// happens.
var (
	_ Publisher                    = (*PubSub)(nil)
	_ transporthttp.SnapshotPubSub = (*PubSub)(nil)
)

// NewPubSub constructs a fresh PubSub. The returned value is ready to
// Subscribe / Publish immediately; no Start step is required.
func NewPubSub() *PubSub {
	return &PubSub{
		subscribers: make(map[pubSubKey][]*pubSubChan),
	}
}

// Subscribe returns a per-subscriber receive channel and a cancel
// function. The channel is buffered (pubSubBufferSize); a slow consumer
// observes dropped snapshots rather than blocking Publish.
//
// The returned cancel is idempotent — multiple invocations are safe.
// The implementation closes the channel exactly once, on the first
// cancel call. After cancel returns, the channel will eventually
// close (Publish bails when it observes the closed flag).
//
// Calling Subscribe after Close returns a closed channel and a no-op
// cancel — graceful behaviour for shutdown races where a new WS
// handshake races against module Stop.
func (p *PubSub) Subscribe(tenantID, operatorID uuid.UUID) (<-chan api.Snapshot, func()) {
	sub := &pubSubChan{ch: make(chan api.Snapshot, pubSubBufferSize)}
	key := pubSubKey{tenantID: tenantID, operatorID: operatorID}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		// Close the channel right away so the caller's select observes
		// the closure and returns.
		close(sub.ch)
		return sub.ch, func() {}
	}
	p.subscribers[key] = append(p.subscribers[key], sub)
	p.mu.Unlock()

	cancel := func() {
		sub.once.Do(func() {
			p.mu.Lock()
			// Only mutate the slice when the parent isn't already
			// closing — Close() already closed every channel in that
			// case.
			if !p.closed {
				slot := p.subscribers[key]
				out := slot[:0]
				for _, c := range slot {
					if c != sub {
						out = append(out, c)
					}
				}
				if len(out) == 0 {
					delete(p.subscribers, key)
				} else {
					p.subscribers[key] = out
				}
			}
			p.mu.Unlock()
			// Close the channel after the map fix-up so a concurrent
			// Publish that already passed the closed-flag check still
			// observes a half-closed channel and skips via the
			// non-blocking send default branch.
			close(sub.ch)
		})
	}
	return sub.ch, cancel
}

// Publish fans snap to every subscriber registered under
// (snap.TenantID, snap.OperatorID). Non-blocking per-subscriber: a
// full buffer drops the snapshot for that subscriber only.
//
// Holds only the read lock so concurrent Publish calls do not
// serialise. The map lookup is O(1) and the per-subscriber loop is
// bounded by the number of WS clients for that operator — typically
// 1, sometimes 2 (operator + supervisor monitoring the same operator
// from a separate tab).
func (p *PubSub) Publish(snap api.Snapshot) {
	key := pubSubKey{tenantID: snap.TenantID, operatorID: snap.OperatorID}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return
	}
	subs := p.subscribers[key]
	if len(subs) == 0 {
		p.mu.RUnlock()
		return
	}
	// Snapshot the slice under the read lock so we can drop the lock
	// before doing the non-blocking sends. This avoids holding the
	// lock across user-controlled goroutine activity (the receiver may
	// be blocked behind a syscall on the WS write).
	copies := make([]*pubSubChan, len(subs))
	copy(copies, subs)
	p.mu.RUnlock()

	for _, c := range copies {
		// Non-blocking send: if the per-subscriber buffer is full, we
		// drop the snapshot for that subscriber. The default branch
		// also covers the closed-channel race: if the cancel function
		// closed c.ch between the snapshot and our send, the sub-case
		// expression panics on send-to-closed; we guard against that
		// via recover.
		safeSend(c.ch, snap)
	}
}

// safeSend performs a non-blocking send and absorbs the panic that
// occurs when ch is already closed. The PubSub guarantees Subscribe's
// cancel closes the channel only once; a concurrent Publish that
// captured the channel before the close still races with the close
// call. Recovering from the resulting panic is simpler than holding
// the write lock across the entire send loop (which would serialise
// every Publish against every Subscribe / cancel).
func safeSend(ch chan api.Snapshot, snap api.Snapshot) {
	defer func() {
		// Send-on-closed-channel panics with "send on closed channel".
		// We discard the recovered value — the snapshot is intentionally
		// lost when the subscription has already torn down.
		_ = recover()
	}()
	select {
	case ch <- snap:
	default:
		// Buffer full → drop. Plan 11 NATS-backed implementation can
		// emit a metric tick when this happens; for now the dropped
		// snapshot is a benign correctness loss (the next FSM
		// transition publishes a fresher snapshot).
	}
}

// Close terminates every active subscription. After Close, every
// previously-Subscribed channel is closed and Publish becomes a no-op.
// Idempotent — multiple Close calls are safe.
//
// Used by Module.Stop on graceful shutdown.
func (p *PubSub) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	subs := p.subscribers
	p.subscribers = nil
	p.mu.Unlock()

	for _, slot := range subs {
		for _, c := range slot {
			c.once.Do(func() {
				close(c.ch)
			})
		}
	}
}

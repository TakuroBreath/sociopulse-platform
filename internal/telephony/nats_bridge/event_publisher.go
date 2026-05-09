package nats_bridge //nolint:revive // package name mirrors the module's filesystem path

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	telapi "github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/internal/telephony/pool"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// publishTimeout caps every per-event NATS publish so a stalled broker
// cannot wedge the event loop. Carry-forward of the bounded-ctx pattern
// used in pool.healthProbe; chosen short (2s) because publish latency on a
// healthy intra-AZ NATS cluster is sub-10ms — anything longer is a real
// fault we want surfaced as a drop, not absorbed.
const publishTimeout = 2 * time.Second

// eventPublisher reads pool.EventEnvelope from the supplied chan and
// publishes JSON-marshalled api.ChannelEvent payloads to NATS via the
// supplied eventbus.Publisher. The loop is resilient to transient publish
// errors (drop + tick metric + continue); it exits only when ctx is
// cancelled or the events chan closes.
type eventPublisher struct {
	pub     eventbus.Publisher
	events  <-chan pool.EventEnvelope
	metrics *Metrics
	logger  *zap.Logger

	wg       sync.WaitGroup
	stopOnce sync.Once
}

// newEventPublisher wires an eventPublisher. logger nil-safe; metrics
// nil-safe (observe* methods are nil-tolerant).
func newEventPublisher(pub eventbus.Publisher, events <-chan pool.EventEnvelope, metrics *Metrics, logger *zap.Logger) *eventPublisher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &eventPublisher{
		pub:     pub,
		events:  events,
		metrics: metrics,
		logger:  logger,
	}
}

// Run starts the goroutine. Returns immediately; the caller drives
// shutdown via ctx cancel + Stop. Safe to call once.
//
// The wg.Go pattern (Go 1.25+) carries forward from Plan 09/10 — no
// manual wg.Add(1)/Done.
func (e *eventPublisher) Run(ctx context.Context) {
	e.wg.Go(func() {
		e.loop(ctx)
	})
}

// loop is the goroutine body. Exits when ctx cancels OR events closes.
// Drop-on-error semantics: a NATS publish failure is logged at debug,
// metric'd, and the loop continues — a transient broker hiccup must not
// stall ESL → NATS event delivery.
func (e *eventPublisher) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-e.events:
			if !ok {
				return
			}
			e.publishOne(ctx, env)
		}
	}
}

// publishOne publishes a single envelope. Skips events the pool emitted
// with a zero-valued Type (those are pool.publishEvent's "MapEvent
// returned ok=false" branch — HEARTBEAT, BACKGROUND_JOB, sofia::register
// etc.) — publishing them would yield an invalid subject ending in ".".
//
// PII discipline: subject and event-type are loggable; payload bytes are NOT.
func (e *eventPublisher) publishOne(ctx context.Context, env pool.EventEnvelope) {
	if env.Event.Type == "" {
		return
	}

	payload, err := json.Marshal(env.Event)
	if err != nil {
		e.metrics.observeEventDropped(dropReasonMarshalError)
		e.logger.Debug("nats_bridge: marshal channel event failed; drop",
			zap.String("kind", string(env.Event.Type)),
			zap.Error(err),
		)
		return
	}

	subject := telapi.SubjectChannelEventFor(env.Event.TenantID, env.Event.CallID, string(env.Event.Type))

	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	if err := e.pub.Publish(pubCtx, subject, payload); err != nil {
		e.metrics.observeEventDropped(dropReasonPublishError)
		e.logger.Debug("nats_bridge: publish channel event failed; drop",
			zap.String("subject", subject),
			zap.String("kind", string(env.Event.Type)),
			zap.Error(err),
		)
		return
	}

	e.metrics.observeEventPublished(string(env.Event.Type))
}

// Stop blocks until the goroutine spawned by Run has exited. Idempotent —
// the underlying sync.Once gates a second call so a defer Stop after a
// Drain Stop is safe.
//
// Note: Stop does NOT cancel ctx — that is the caller's responsibility
// (the bridge's Stop holds the cancel func).
func (e *eventPublisher) Stop() {
	e.stopOnce.Do(func() {
		e.wg.Wait()
	})
}

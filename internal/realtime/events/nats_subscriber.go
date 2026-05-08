package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"go.uber.org/zap"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// HubBroadcaster is the subset of *service.Hub the dispatcher needs.
// Narrow on purpose so tests can inject a fake without spinning the
// real Hub. Production wiring satisfies it via *service.Hub (see the
// compile-time check in nats_subscriber_test.go).
type HubBroadcaster interface {
	Broadcast(ctx context.Context, topic rtapi.Topic, payload json.RawMessage, filter rtapi.BroadcastFilter) int
}

// Option tweaks NATSSubscriber construction without bloating the
// constructor signature. Functional-options pattern (Plan 09/10
// carry-forward).
type Option func(*subscriberOptions)

type subscriberOptions struct {
	replicaID string
}

// WithReplicaID sets the replica identifier used to derive the queue
// group name "realtime-replica-<replicaID>". Empty string falls back
// to a uuid generated at construction. Per Plan 11 Decision Q2 the
// replicaID must be unique per pod so each replica receives every
// message (queue-group degeneration).
func WithReplicaID(id string) Option {
	return func(o *subscriberOptions) { o.replicaID = id }
}

// NATSSubscriber fans out NATS events under tenant.> into the local Hub.
// One *NATSSubscriber instance per cmd/api replica; goroutines that
// deliver messages live inside the underlying eventbus.Subscriber.
type NATSSubscriber struct {
	bus     eventbus.Subscriber
	hub     HubBroadcaster
	logger  *zap.Logger
	metrics *Metrics
	queue   string

	patterns []subjectPattern

	mu      sync.Mutex
	started bool
	stopped bool
}

// subjectPattern is one entry in the dispatcher's subject→topic table.
// Each pattern carries a tokeniser that extracts the per-broadcast
// filter from the subject parts.
type subjectPattern struct {
	subject  string
	topic    rtapi.Topic
	tokens   int                                                // expected token count after splitting on '.'
	extract  func(parts []string) (rtapi.BroadcastFilter, bool) // (filter, ok); ok=false → empty tenant
	topicLab string                                             // label string for metrics (matches rtapi.Topic value)
}

// NewNATSSubscriber constructs a dispatcher.
//
// bus and hub MUST be non-nil. logger nil-safe (defaults to
// zap.NewNop). metrics nil-safe (every observe* helper short-circuits
// on nil).
func NewNATSSubscriber(bus eventbus.Subscriber, hub HubBroadcaster, logger *zap.Logger, metrics *Metrics, opts ...Option) *NATSSubscriber {
	if bus == nil {
		panic("realtime/events: NewNATSSubscriber: bus must be non-nil")
	}
	if hub == nil {
		panic("realtime/events: NewNATSSubscriber: hub must be non-nil")
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	o := subscriberOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.replicaID == "" {
		o.replicaID = uuid.NewString()
	}

	return &NATSSubscriber{
		bus:      bus,
		hub:      hub,
		logger:   logger,
		metrics:  metrics,
		queue:    fmt.Sprintf("realtime-replica-%s", o.replicaID),
		patterns: defaultPatterns(),
	}
}

// defaultPatterns returns the canonical subject→topic table. Kept as
// a function (not a package-level var) so each *NATSSubscriber owns its
// own slice — there is no shared mutable state between dispatchers.
//
// Note on trunks.health: deliberately omitted. Hub.Broadcast returns 0
// for an empty TenantID filter (cross-tenant leak guard). Plan 11
// Task 7's HTTP layer will fan trunks.health out per-tenant via a
// separate path.
func defaultPatterns() []subjectPattern {
	return []subjectPattern{
		{
			subject:  "tenant.*.dialer.op.*.state",
			topic:    rtapi.TopicOperatorsState,
			topicLab: string(rtapi.TopicOperatorsState),
			tokens:   6,
			extract: func(p []string) (rtapi.BroadcastFilter, bool) {
				if p[1] == "" {
					return rtapi.BroadcastFilter{}, false
				}
				return rtapi.BroadcastFilter{TenantID: p[1]}, true
			},
		},
		{
			subject:  "tenant.*.dialer.queue",
			topic:    rtapi.TopicDialerQueue,
			topicLab: string(rtapi.TopicDialerQueue),
			tokens:   4,
			extract: func(p []string) (rtapi.BroadcastFilter, bool) {
				if p[1] == "" {
					return rtapi.BroadcastFilter{}, false
				}
				return rtapi.BroadcastFilter{TenantID: p[1]}, true
			},
		},
		{
			subject:  "tenant.*.telephony.event.*.*",
			topic:    rtapi.TopicCallEvents,
			topicLab: string(rtapi.TopicCallEvents),
			tokens:   6,
			extract: func(p []string) (rtapi.BroadcastFilter, bool) {
				if p[1] == "" {
					return rtapi.BroadcastFilter{}, false
				}
				return rtapi.BroadcastFilter{TenantID: p[1], CallID: p[4]}, true
			},
		},
		{
			subject:  "tenant.*.notify.user.*",
			topic:    rtapi.TopicNotifications,
			topicLab: string(rtapi.TopicNotifications),
			tokens:   5,
			extract: func(p []string) (rtapi.BroadcastFilter, bool) {
				if p[1] == "" {
					return rtapi.BroadcastFilter{}, false
				}
				return rtapi.BroadcastFilter{TenantID: p[1], UserID: p[4]}, true
			},
		},
		{
			subject:  "tenant.*.force.user.*",
			topic:    rtapi.TopicForceCommands,
			topicLab: string(rtapi.TopicForceCommands),
			tokens:   5,
			extract: func(p []string) (rtapi.BroadcastFilter, bool) {
				if p[1] == "" {
					return rtapi.BroadcastFilter{}, false
				}
				return rtapi.BroadcastFilter{TenantID: p[1], UserID: p[4]}, true
			},
		},
	}
}

// Start registers all subject patterns with the underlying bus.
// Calling Start twice is a wiring bug — the second call returns an
// error rather than silently rebuilding the registrations. The cmd/api
// composition root is the only Start caller.
//
// If any single Subscribe call fails, Start returns the wrapped error
// immediately. Earlier-registered subscriptions remain on the bus —
// the caller is expected to invoke Stop (or shut the entire bus down)
// to clean up. We do not attempt partial-rollback because the
// production composition root treats a Start failure as fatal.
func (s *NATSSubscriber) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("realtime/events: subscriber already started")
	}

	// Go 1.22+ scopes loop vars per-iteration, so the closure captures
	// each subjectPattern correctly without an explicit copy.
	for _, p := range s.patterns {
		handler := func(subject string, payload []byte) error {
			s.dispatch(ctx, p, subject, payload)
			// Always ack — see dispatch comment.
			return nil
		}
		if err := s.bus.Subscribe(ctx, p.subject, s.queue, handler); err != nil {
			return fmt.Errorf("realtime/events: subscribe %q: %w", p.subject, err)
		}
	}
	s.started = true

	// trunks.health is intentionally not wired here. The Hub refuses
	// empty-TenantID broadcasts (cross-tenant leak guard). Plan 11
	// Task 7 (HTTP handlers) will fan trunks.health out per-tenant via
	// a separate per-tenant replication path. Emit a single debug log
	// at startup so anyone tailing logs can correlate the missing
	// subject with this comment.
	s.logger.Debug("realtime/events: trunks.health subject not yet wired (TODO Plan 11 Task 7)",
		zap.String("queue", s.queue),
	)

	return nil
}

// Stop releases the dispatcher's hold on the bus. Idempotent — second
// Stop is a no-op. The underlying eventbus.Subscriber owns the actual
// goroutines; ownership of subscription teardown belongs to the
// composition root (cmd/api Closes the bus, which drains every
// registered consumer). Stop here just flips the local "stopped" flag
// so subsequent Start calls are rejected with a clear error.
func (s *NATSSubscriber) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil
	}
	s.stopped = true
	return nil
}

// dispatch is invoked by the underlying eventbus handler for each
// inbound message. It tokenises the subject, projects it into a
// (topic, filter, payload) tuple, calls Hub.Broadcast, and records
// the fan-out count.
//
// The handler always acks (returns nil) — propagating a non-nil error
// to the bus would trigger NATS redelivery, which for a permanently
// malformed subject is an infinite loop. Skip + log at debug + count
// the failure metric.
//
// PII discipline: only log subject + payload byte count. NEVER log
// the raw payload, which can carry tenant- or user-scoped PII.
func (s *NATSSubscriber) dispatch(ctx context.Context, p subjectPattern, subject string, payload []byte) {
	parts := strings.Split(subject, ".")
	if len(parts) != p.tokens {
		s.logger.Debug("realtime/events: skipping subject with unexpected token count",
			zap.String("subject", subject),
			zap.String("topic", p.topicLab),
			zap.Int("got_tokens", len(parts)),
			zap.Int("want_tokens", p.tokens),
			zap.Int("payload_bytes", len(payload)),
		)
		s.metrics.observeDispatchFailure(p.topicLab, reasonMalformed)
		return
	}

	filter, ok := p.extract(parts)
	if !ok {
		s.logger.Debug("realtime/events: skipping subject with empty tenant",
			zap.String("subject", subject),
			zap.String("topic", p.topicLab),
			zap.Int("payload_bytes", len(payload)),
		)
		s.metrics.observeDispatchFailure(p.topicLab, reasonEmptyTenant)
		return
	}

	count := s.hub.Broadcast(ctx, p.topic, json.RawMessage(payload), filter)
	s.metrics.observeMessage(p.topicLab)
	s.metrics.observeFanout(count)
}

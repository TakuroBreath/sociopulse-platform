// Package nats holds the billing module's NATS subscribers. Currently:
//   - CallFinalizedSubscriber consumes tenant.*.dialer.call.finalized
//     events and invokes billingapi.CallFinalizedHook.OnCallFinalized.
//
// Subscription pattern follows internal/dialer/transport/nats — wildcard
// subject + named queue group so multiple cmd/api replicas load-balance
// the message stream. Idempotency is enforced by the hook
// (ON CONFLICT (call_id) DO NOTHING in call_costs), so JetStream
// redeliveries from a handler-returned error are safe.
package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

const (
	// SubscribeSubject is the wildcard the subscriber binds on the bus.
	// The bus delivers every per-tenant
	// `tenant.<uuid>.dialer.call.finalized` message to this single
	// subscription; the queue-group load-balances across replicas.
	SubscribeSubject = "tenant.*.dialer.call.finalized"

	// QueueGroup is the NATS queue-group name. Each message is delivered
	// to exactly one member of the group, so horizontal scaling of cmd/api
	// (or cmd/worker, if the subscriber moves there later) does not
	// duplicate billing work.
	QueueGroup = "billing-call-finalized"
)

// CallFinalizedSubscriber decodes dialer.CallFinalizedEvent payloads and
// invokes the billing CallFinalizedHook. Handler errors trigger NATS
// redelivery; the underlying hook's idempotent INSERT (ON CONFLICT)
// tolerates duplicates so redelivery is safe.
//
// Stateless apart from its collaborators — safe to construct as a value,
// share, and discard. Matches the canonical pattern from
// internal/dialer/transport/nats.CallEventSubscriber.
type CallFinalizedSubscriber struct {
	hook   billingapi.CallFinalizedHook
	logger *zap.Logger
}

// NewCallFinalizedSubscriber wires the subscriber. hook MUST be non-nil
// — wiring without it is a programmer bug and we fail loud (panic) rather
// than silently no-op every event in production. logger is nil-tolerant
// (defaults to zap.NewNop) so tests need not construct one.
func NewCallFinalizedSubscriber(hook billingapi.CallFinalizedHook, logger *zap.Logger) *CallFinalizedSubscriber {
	if hook == nil {
		panic("billing/transport/nats: NewCallFinalizedSubscriber: hook must be non-nil")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &CallFinalizedSubscriber{hook: hook, logger: logger}
}

// Subscribe registers the wildcard subscription on the supplied bus.
// Returns the bus's Subscribe error — non-nil typically means the bus
// is closed or the subject/queue are malformed. Subsequent message
// handling is asynchronous on the bus's dispatcher; this function
// returns once the subscription is registered.
func (s *CallFinalizedSubscriber) Subscribe(ctx context.Context, bus eventbus.Subscriber) error {
	if bus == nil {
		return errors.New("billing/transport/nats: Subscribe: bus must be non-nil")
	}
	if err := bus.Subscribe(ctx, SubscribeSubject, QueueGroup, s.handle); err != nil {
		return fmt.Errorf("billing/transport/nats: subscribe %q: %w", SubscribeSubject, err)
	}
	s.logger.Info("billing call-finalized subscriber registered",
		zap.String("subject", SubscribeSubject),
		zap.String("queue", QueueGroup),
	)
	return nil
}

// handle is the bus push-consumer entry point. Returns nil to ACK or a
// non-nil error to NAK (triggering NATS redelivery). The hook is
// idempotent so duplicates are harmless.
//
// The handler signature does not carry a context (eventbus.Subscriber's
// contract is `func(subject string, payload []byte) error`). We use
// context.Background() for the hook invocation — NATS deliveries have
// their own lifecycle independent of any HTTP request, and a finalize
// write is durable state we must not abandon mid-Tx. If a future
// refactor adds ctx to the handler signature, propagate it here.
func (s *CallFinalizedSubscriber) handle(subject string, payload []byte) error {
	var raw dialerapi.CallFinalizedEvent
	if err := json.Unmarshal(payload, &raw); err != nil {
		// Bad JSON is unrecoverable on redelivery. Return the error so
		// the bus's max-deliver policy eventually drops the message;
		// log at WARN so an operator can spot the poison source.
		s.logger.Warn("billing/subscriber: decode failed",
			zap.String("subject", subject),
			zap.Error(err),
		)
		return fmt.Errorf("billing/subscriber: decode: %w", err)
	}
	if raw.CallID == uuid.Nil {
		s.logger.Warn("billing/subscriber: missing call_id",
			zap.String("subject", subject),
		)
		return errors.New("billing/subscriber: missing call_id")
	}
	if raw.TenantID == uuid.Nil {
		s.logger.Warn("billing/subscriber: missing tenant_id",
			zap.String("subject", subject),
			zap.String("call_id", raw.CallID.String()),
		)
		return errors.New("billing/subscriber: missing tenant_id")
	}

	in := billingapi.CallCostInput{
		CallID:       raw.CallID,
		TenantID:     raw.TenantID,
		ProjectID:    raw.ProjectID,
		TrunkUsed:    raw.TrunkUsed,
		DurationSec:  raw.DurationSec,
		Status:       raw.Status,
		StorageBytes: raw.StorageBytes,
		// FinalizedAt is unix seconds on the wire (per
		// internal/dialer/api/events.go) — convert to UTC time.Time for
		// the hook (see docs/references/plan-14-billing.md §2.3).
		FinalizedAt: time.Unix(raw.FinalizedAt, 0).UTC(),
	}
	// OnCallFinalized is idempotent. Returning the error redelivers; on
	// success (including the idempotent ON CONFLICT skip) returning nil
	// ACKs.
	if err := s.hook.OnCallFinalized(context.Background(), in); err != nil {
		s.logger.Warn("billing/subscriber: handle failed",
			zap.String("subject", subject),
			zap.String("call_id", raw.CallID.String()),
			zap.String("tenant_id", raw.TenantID.String()),
			zap.Error(err),
		)
		return fmt.Errorf("billing/subscriber: handle: %w", err)
	}
	return nil
}

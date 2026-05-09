package nats_bridge //nolint:revive // package name mirrors the module's filesystem path

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	telapi "github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// cmdQueue is the queue group used for cmd-subscriber subscriptions. Every
// telephony-bridge replica joins the same group so JetStream load-balances
// inbound commands across exactly one consumer at a time.
const cmdQueue = "telephony-bridge"

// cmdSubject is the wildcard subject the bridge subscribes to. The leading
// "tenant.*" matches every tenant; the trailing ">" matches every per-call
// callID under "telephony.cmd.". The exact subject the dialer publishes
// against is built by telapi.SubjectCommandFor.
const cmdSubject = "tenant.*.telephony.cmd.>"

// commandEnvelope is the JSON shape inbound command messages take on the
// bus. The dialer (Plan 10 transport-http command publisher) marshals this
// shape; the bridge decodes it. Args is left as json.RawMessage so the
// bridge can decode the kind-specific sub-DTO only after dispatch
// resolution.
type commandEnvelope struct {
	// CommandID is the idempotency key. Must be a stable string so a
	// publisher-side replay produces the same key (the Plan 10 dialer
	// uses uuid.NewString() for this).
	CommandID string `json:"command_id"`
	// Kind is one of {"originate", "hangup", "mixmonitor.start",
	// "mixmonitor.stop"} — see the kind* consts in metrics.go.
	Kind string `json:"kind"`
	// TenantID is the per-tenant scope the command operates within. The
	// bridge does NOT today re-derive it from the subject (defence in
	// depth would; we trust the caller for now and call it out here).
	TenantID string `json:"tenant_id"`
	// CallID is the per-call scope. Empty for originate (FS picks the
	// uuid); populated for hangup and mixmonitor.*.
	CallID string `json:"call_id"`
	// Node is the FS ESL node addr the dispatcher should target. The
	// dialer's router resolves this upstream (Plan 09 Task 5).
	Node string `json:"node"`
	// Args is the kind-specific sub-DTO. Left as RawMessage so we can
	// decode lazily after kind dispatch.
	Args json.RawMessage `json:"args"`
}

// poolDispatcher narrows *pool.ESLPool (or rather, the wrapper around it
// that resolves a node into an *esl.Client and calls the command) to just
// what cmd_subscriber needs. Tests inject a fake without depending on the
// concrete pool fleet.
type poolDispatcher interface {
	Originate(ctx context.Context, node string, cmd telapi.OriginateCommand) error
	Hangup(ctx context.Context, node, callID, cause string) error
	MixMonitorStart(ctx context.Context, node, callID, path string) error
	MixMonitorStop(ctx context.Context, node, callID string) error
}

// idempotencyChecker is the narrow surface cmdSubscriber needs from the
// guard. Defining it as an interface (not a *IdempotencyGuard pointer)
// keeps the cmd_subscriber tests from needing a miniredis — they swap in
// a struct that returns whatever the test needs.
type idempotencyChecker interface {
	MarkSeen(ctx context.Context, commandID string) (bool, error)
}

// cmdSubscriber is the inbound command pipeline: it subscribes to the
// cmd subject, decodes envelopes, idempotency-checks, and dispatches to
// the ESL pool via poolDispatcher.
type cmdSubscriber struct {
	bus     eventbus.Subscriber
	pool    poolDispatcher
	guard   idempotencyChecker
	metrics *Metrics
	logger  *zap.Logger
}

// newCmdSubscriber wires a cmdSubscriber. Logger nil-safe; metrics
// nil-safe (observe* methods are nil-tolerant).
func newCmdSubscriber(bus eventbus.Subscriber, pool poolDispatcher, guard idempotencyChecker, metrics *Metrics, logger *zap.Logger) *cmdSubscriber {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &cmdSubscriber{
		bus:     bus,
		pool:    pool,
		guard:   guard,
		metrics: metrics,
		logger:  logger,
	}
}

// Start registers the handle method against the cmd subject under the
// telephony-bridge queue group. ctx is honoured for the registration step
// only — push-consumer goroutines are owned by the bus implementation.
func (c *cmdSubscriber) Start(ctx context.Context) error {
	if c.bus == nil {
		return fmt.Errorf("nats_bridge: cmd subscriber: bus is nil")
	}
	if err := c.bus.Subscribe(ctx, cmdSubject, cmdQueue, c.handle); err != nil {
		return fmt.Errorf("nats_bridge: subscribe %q: %w", cmdSubject, err)
	}
	return nil
}

// handle is the per-message hook. The signature matches eventbus.Subscriber's
// handler shape (subject, payload, error). The handler returns:
//
//   - nil        → bus acks (success OR a permanent reason like duplicate /
//     malformed / unknown_kind — never redeliver those).
//   - non-nil    → bus NAKs (Redis-down or pool-error → redeliver after
//     the bus's nak-delay).
//
// PII discipline: payload bytes are NEVER logged — we surface command_id,
// kind, and node only.
func (c *cmdSubscriber) handle(_ string, payload []byte) error {
	var env commandEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		c.metrics.observeCommandRejected(rejectReasonMalformed)
		c.logger.Debug("nats_bridge: drop malformed command envelope",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}

	c.metrics.observeCommandReceived(env.Kind)

	// Idempotency-check FIRST — we want a duplicate to never reach
	// the pool, regardless of kind. A Redis-side error here MUST
	// surface so the bus NAKs (silent dedup-failure would let
	// commands be lost on a publisher-replay storm).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen, err := c.guard.MarkSeen(ctx, env.CommandID)
	if err != nil {
		c.metrics.observeCommandRejected(rejectReasonIdempotencyError)
		c.logger.Debug("nats_bridge: idempotency check failed; NACK",
			zap.String("command_id", env.CommandID),
			zap.String("kind", env.Kind),
			zap.Error(err),
		)
		return fmt.Errorf("nats_bridge: idempotency: %w", err)
	}
	if !seen {
		c.metrics.observeCommandRejected(rejectReasonDuplicate)
		c.logger.Debug("nats_bridge: duplicate command, ack-skip",
			zap.String("command_id", env.CommandID),
			zap.String("kind", env.Kind),
		)
		return nil
	}

	if err := c.dispatch(ctx, env); err != nil {
		c.logger.Debug("nats_bridge: dispatch failed; NACK",
			zap.String("command_id", env.CommandID),
			zap.String("kind", env.Kind),
			zap.String("node", env.Node),
			zap.Error(err),
		)
		c.metrics.observeCommandRejected(rejectReasonPoolError)
		return fmt.Errorf("nats_bridge: dispatch %q: %w", env.Kind, err)
	}
	return nil
}

// dispatch routes the decoded envelope to the right poolDispatcher method.
// Returns nil for malformed-args (logged + acked at the caller via the
// rejection metric path). Returns the dispatcher error verbatim for
// pool-side failures so the caller maps it to NACK.
func (c *cmdSubscriber) dispatch(ctx context.Context, env commandEnvelope) error {
	switch env.Kind {
	case kindOriginate:
		var cmd telapi.OriginateCommand
		if err := json.Unmarshal(env.Args, &cmd); err != nil {
			c.metrics.observeCommandRejected(rejectReasonMalformed)
			c.logger.Debug("nats_bridge: malformed originate args; ack-skip",
				zap.String("command_id", env.CommandID),
				zap.Error(err),
			)
			return nil
		}
		return c.pool.Originate(ctx, env.Node, cmd)

	case kindHangup:
		var args struct {
			Cause string `json:"cause"`
		}
		if err := json.Unmarshal(env.Args, &args); err != nil {
			c.metrics.observeCommandRejected(rejectReasonMalformed)
			c.logger.Debug("nats_bridge: malformed hangup args; ack-skip",
				zap.String("command_id", env.CommandID),
				zap.Error(err),
			)
			return nil
		}
		return c.pool.Hangup(ctx, env.Node, env.CallID, args.Cause)

	case kindMixmonitorStart:
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(env.Args, &args); err != nil {
			c.metrics.observeCommandRejected(rejectReasonMalformed)
			c.logger.Debug("nats_bridge: malformed mixmonitor.start args; ack-skip",
				zap.String("command_id", env.CommandID),
				zap.Error(err),
			)
			return nil
		}
		return c.pool.MixMonitorStart(ctx, env.Node, env.CallID, args.Path)

	case kindMixmonitorStop:
		return c.pool.MixMonitorStop(ctx, env.Node, env.CallID)

	default:
		c.metrics.observeCommandRejected(rejectReasonUnknownKind)
		c.logger.Debug("nats_bridge: unknown command kind; ack-skip",
			zap.String("command_id", env.CommandID),
			zap.String("kind", env.Kind),
		)
		return nil
	}
}

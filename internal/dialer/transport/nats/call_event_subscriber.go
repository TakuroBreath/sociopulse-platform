// Package nats provides the NATS-side transport for the dialer module.
//
// The CallEventSubscriber consumes telephony channel events published by
// cmd/telephony-bridge (via internal/telephony/nats_bridge) on
// tenant.<t>.telephony.event.<call_id>.<type> and forwards them to the
// OperatorFSM. This closes Plan 13.2.5 Task 2's CRITICAL finding: the
// dialer FSM cannot leave dialing/call states from telephony events
// because the consumer was never wired — Plan 10 declared
// dialer.api.Router.Subscribe but no module called it.
//
// Routing table:
//
//	Type=answer  → OperatorFSM.RecordCallStarted (dialing → call)
//	Type=hangup  → OperatorFSM.RecordCallEnded   (call|dialing → status)
//	other types  → ack + skip (analytics + recording own those)
//
// Outcome derivation from FreeSWITCH hangup_cause (per Plan 13.2.5 Task 2):
//
//	NORMAL_CLEARING                                  → OutcomeSuccess
//	USER_BUSY                                        → OutcomeBusy
//	NO_ANSWER, NO_USER_RESPONSE, ALLOTTED_TIMEOUT    → OutcomeNoAnswer
//	CALL_REJECTED, NORMAL_TEMPORARY_FAILURE, ...     → OutcomeTechFailure
//	anything unrecognised (incl. empty)              → OutcomeTechFailure
//
// Idempotency: NATS JetStream is at-least-once. When the FSM rejects a
// transition with api.ErrInvalidTransition (typically because the
// operator is already in the target state due to a redelivery) we
// ACK + log at debug rather than NAK — looping would never converge.
// Any other FSM error (transient Redis/PG outage) is propagated so the
// bus NAKs with delay and JetStream retries.
//
// Operator resolution: ChannelEvent carries (tenant_id, call_id) but no
// operator_id. The subscriber depends on CallOperatorLookup — a small
// consumer-side port — to resolve operator_id from the calls table.
// cmd/worker wires a PG-backed implementation; tests provide an
// in-memory fake.
package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	telapi "github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// SubscribeSubject is the wildcard the CallEventSubscriber binds on the
// bus. Matches every per-tenant per-call telephony event; the handler
// then dispatches on the ChannelEvent.Type field. Held in a constant so
// integration tests can provision a JetStream stream for the same
// subject pattern without re-deriving it.
const SubscribeSubject = "tenant.*.telephony.event.>"

// DefaultQueueGroup is the JetStream queue group name. Per-replica
// queue groups would degenerate to "every replica receives every
// message" (Plan 11 Decision Q2); shared "dialer-call-events" gives
// load-balanced delivery across cmd/worker replicas.
const DefaultQueueGroup = "dialer-call-events"

// ErrOperatorNotFound is returned by CallOperatorLookup when no row
// exists for the (tenant_id, call_id) pair. The subscriber treats this
// as a dead-letter condition: a telephony event for a call that has
// no operator row in PG cannot meaningfully transition any FSM, so
// retrying via JetStream would loop without convergence.
var ErrOperatorNotFound = errors.New("dialer/transport/nats: operator not found for call")

// CallOperatorLookup resolves (tenant_id, call_id) → operator_id by
// consulting the dialer's authoritative store. cmd/worker wires a
// Postgres-backed implementation; tests provide an in-memory fake.
//
// Returns ErrOperatorNotFound when the call row is missing (dead-letter
// signal); any other error indicates a transient backend fault and the
// subscriber NAKs the message for JetStream redelivery.
type CallOperatorLookup interface {
	LookupOperator(ctx context.Context, tenantID, callID uuid.UUID) (uuid.UUID, error)
}

// CallEventSubscriber is the bus-side adapter that turns telephony
// channel events into OperatorFSM calls. Stateless apart from its
// collaborators — safe to construct as a value, share, and discard.
type CallEventSubscriber struct {
	fsm    dialerapi.OperatorFSM
	lookup CallOperatorLookup
	logger *zap.Logger
}

// NewCallEventSubscriber constructs the subscriber. fsm and lookup
// MUST be non-nil — wiring without either is a programmer bug and we
// fail loud (panic) rather than silently no-op every event in
// production. logger is nil-tolerant (defaults to a nop).
func NewCallEventSubscriber(fsm dialerapi.OperatorFSM, lookup CallOperatorLookup, logger *zap.Logger) *CallEventSubscriber {
	if fsm == nil {
		panic("dialer/transport/nats: NewCallEventSubscriber: fsm must be non-nil")
	}
	if lookup == nil {
		panic("dialer/transport/nats: NewCallEventSubscriber: lookup must be non-nil")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &CallEventSubscriber{
		fsm:    fsm,
		lookup: lookup,
		logger: logger,
	}
}

// Subscribe registers the per-call event handler on the supplied bus.
// queue is the JetStream queue-group name — pass DefaultQueueGroup
// unless an integration test needs isolation. Returns when Subscribe
// returns; teardown is owned by the bus's Close (called by cmd/worker
// on shutdown).
//
// The subscriber registers a SINGLE handler on the wildcard subject
// and dispatches on ChannelEvent.Type. This is intentional: a per-type
// subscription set would force the bus to maintain N consumers per
// tenant per call, multiplying broker state without buying us
// per-handler observability we don't have today.
func (s *CallEventSubscriber) Subscribe(ctx context.Context, bus eventbus.Subscriber, queue string) error {
	if bus == nil {
		return errors.New("dialer/transport/nats: Subscribe: bus must be non-nil")
	}
	if queue == "" {
		queue = DefaultQueueGroup
	}
	if err := bus.Subscribe(ctx, SubscribeSubject, queue, s.handle); err != nil {
		return fmt.Errorf("dialer/transport/nats: subscribe %q: %w", SubscribeSubject, err)
	}
	s.logger.Info("call-event subscriber registered",
		zap.String("subject", SubscribeSubject),
		zap.String("queue", queue),
	)
	return nil
}

// handle is the bus push-consumer entry point. Returns nil to ACK or a
// non-nil error to NAK (triggering JetStream redelivery after the
// subscriber's nakDelay). The single-handling rule applies: this is
// the OUTERMOST handler in the dispatch chain, so all errors are
// logged here once and either returned (transient — NAK) or swallowed
// (dead-letter — ACK).
//
// Subject is parsed only for diagnostics; the routing key is the
// payload's Type field. TenantID + CallID come from the payload, NOT
// the subject — telephony's bridge owns the canonical projection.
func (s *CallEventSubscriber) handle(subject string, payload []byte) error {
	var ev telapi.ChannelEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		// Poison message: a malformed payload cannot become well-formed
		// on redelivery. ACK + log at debug so flaky publishers don't
		// flood info-level logs.
		s.logger.Debug("dialer/transport/nats: malformed telephony event — dead-letter",
			zap.String("subject", subject),
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}

	if ev.TenantID == uuid.Nil || ev.CallID == uuid.Nil {
		// Producer bug: telephony's bridge populates both fields. ACK +
		// log at debug; redelivery would not heal the sentinel.
		s.logger.Debug("dialer/transport/nats: telephony event missing tenant/call id — dead-letter",
			zap.String("subject", subject),
			zap.String("tenant_id", ev.TenantID.String()),
			zap.String("call_id", ev.CallID.String()),
			zap.String("event_type", string(ev.Type)),
		)
		return nil
	}

	switch ev.Type {
	case telapi.EventAnswer:
		// Detached background ctx so the FSM commit completes even when
		// the bus dispatcher cancels its push ctx during shutdown — the
		// FSM write is durable state we must not abandon mid-Tx.
		return s.handleAnswered(context.Background(), subject, ev)
	case telapi.EventHangup:
		return s.handleHangup(context.Background(), subject, ev)
	case telapi.EventDialing,
		telapi.EventBridge,
		telapi.EventUnbridge,
		telapi.EventDTMF,
		telapi.EventRecordStop:
		// Not on the FSM critical path — analytics + recording own
		// these. ACK silently to keep the broker pointer advancing.
		return nil
	default:
		// Unknown type — likely a new event the bridge added without
		// updating this dispatch. Ack so we don't loop; log so it's
		// visible in the bus's `nats consumer report`.
		s.logger.Debug("dialer/transport/nats: unrecognised telephony event type — dead-letter",
			zap.String("subject", subject),
			zap.String("event_type", string(ev.Type)),
		)
		return nil
	}
}

// handleAnswered routes a Type=answer event to OperatorFSM.RecordCallStarted.
// Resolves operator_id via the lookup port, then invokes the FSM. The
// FSM is idempotent on a same-call_id replay (machine.go documents the
// short-circuit), so a duplicate delivery is a benign re-traversal of
// the call edge. An api.ErrInvalidTransition from any other cause
// (e.g. operator already offline) is still treated as a dead-letter
// because retrying it would never converge — the operator's state
// drifted and we cannot rewind from this telephony signal alone.
func (s *CallEventSubscriber) handleAnswered(ctx context.Context, subject string, ev telapi.ChannelEvent) error {
	operatorID, err := s.lookup.LookupOperator(ctx, ev.TenantID, ev.CallID)
	if err != nil {
		return s.handleLookupError(subject, ev, err)
	}

	req := dialerapi.CallStartedRequest{
		TenantID:   ev.TenantID,
		OperatorID: operatorID,
		CallID:     ev.CallID,
		StartedAt:  ev.Timestamp,
	}
	if _, err := s.fsm.RecordCallStarted(ctx, req); err != nil {
		return s.handleFSMError(subject, ev, operatorID, "RecordCallStarted", err)
	}
	return nil
}

// handleHangup routes a Type=hangup event to OperatorFSM.RecordCallEnded
// with an Outcome derived from ev.HangupCause via outcomeForHangupCause.
func (s *CallEventSubscriber) handleHangup(ctx context.Context, subject string, ev telapi.ChannelEvent) error {
	operatorID, err := s.lookup.LookupOperator(ctx, ev.TenantID, ev.CallID)
	if err != nil {
		return s.handleLookupError(subject, ev, err)
	}

	outcome := outcomeForHangupCause(ev.HangupCause)
	req := dialerapi.CallEndedRequest{
		TenantID:   ev.TenantID,
		OperatorID: operatorID,
		CallID:     ev.CallID,
		EndedAt:    ev.Timestamp,
		Cause:      ev.HangupCause,
		DurationMS: int(ev.DurationMS), //nolint:gosec // DurationMS is bridge-supplied and bounded by call duration
		Outcome:    outcome,
	}
	if _, err := s.fsm.RecordCallEnded(ctx, req); err != nil {
		return s.handleFSMError(subject, ev, operatorID, "RecordCallEnded", err)
	}
	return nil
}

// handleLookupError classifies a CallOperatorLookup failure into
// dead-letter (ACK + log) or transient (return + NAK).
// ErrOperatorNotFound is permanent — the calls row is missing and no
// retry will heal it. Anything else is treated as transient.
func (s *CallEventSubscriber) handleLookupError(subject string, ev telapi.ChannelEvent, err error) error {
	if errors.Is(err, ErrOperatorNotFound) {
		s.logger.Debug("dialer/transport/nats: operator lookup miss — dead-letter",
			zap.String("subject", subject),
			zap.String("tenant_id", ev.TenantID.String()),
			zap.String("call_id", ev.CallID.String()),
			zap.String("event_type", string(ev.Type)),
		)
		return nil
	}
	// Transient (DB outage, network) — NAK for redelivery. Logged at
	// debug because the bus already metric-tracks NAKs.
	s.logger.Debug("dialer/transport/nats: operator lookup transient error — NAK",
		zap.String("subject", subject),
		zap.String("tenant_id", ev.TenantID.String()),
		zap.String("call_id", ev.CallID.String()),
		zap.Error(err),
	)
	return fmt.Errorf("dialer/transport/nats: lookup operator: %w", err)
}

// handleFSMError classifies an OperatorFSM call failure:
//
//   - api.ErrInvalidTransition → dead-letter (already in target state,
//     or FSM drift the telephony signal can't repair).
//   - api.ErrConflict          → dead-letter (optimistic concurrency
//     conflict — the FSM has been advanced by a concurrent operator
//     action; the telephony signal is stale).
//   - everything else          → transient (NAK for redelivery).
//
// fsmMethod is the name of the FSM method called ("RecordCallStarted"
// / "RecordCallEnded") and feeds the diagnostic log field; it's the
// only variable data in the log message so the log-aggregator
// cardinality stays bounded.
func (s *CallEventSubscriber) handleFSMError(subject string, ev telapi.ChannelEvent, operatorID uuid.UUID, fsmMethod string, err error) error {
	if errors.Is(err, dialerapi.ErrInvalidTransition) || errors.Is(err, dialerapi.ErrConflict) {
		s.logger.Debug("dialer/transport/nats: FSM rejected transition — dead-letter",
			zap.String("subject", subject),
			zap.String("tenant_id", ev.TenantID.String()),
			zap.String("operator_id", operatorID.String()),
			zap.String("call_id", ev.CallID.String()),
			zap.String("event_type", string(ev.Type)),
			zap.String("fsm_method", fsmMethod),
			zap.Error(err),
		)
		return nil
	}
	s.logger.Debug("dialer/transport/nats: FSM transient error — NAK",
		zap.String("subject", subject),
		zap.String("tenant_id", ev.TenantID.String()),
		zap.String("operator_id", operatorID.String()),
		zap.String("call_id", ev.CallID.String()),
		zap.String("event_type", string(ev.Type)),
		zap.String("fsm_method", fsmMethod),
		zap.Error(err),
	)
	return fmt.Errorf("dialer/transport/nats: %s: %w", fsmMethod, err)
}

// outcomeForHangupCause classifies a FreeSWITCH hangup_cause into a
// dialer-API StatusOutcome. The mapping is the single source of truth
// for "what survey outcome did this call attempt produce" given only
// the telephony termination signal.
//
// Unrecognised causes (including the empty string) fall to
// OutcomeTechFailure — the fail-safe choice because techn-failure is
// the only outcome that does NOT grant verify-class privilege, so a
// misclassified call cannot accidentally permit the (status, go_verify)
// → verify transition.
//
// References: FreeSWITCH cause-codes reference
// (https://freeswitch.org/confluence/display/FREESWITCH/Hangup+Cause+Code+Table).
func outcomeForHangupCause(cause string) dialerapi.StatusOutcome {
	switch cause {
	case "NORMAL_CLEARING":
		return dialerapi.OutcomeSuccess
	case "USER_BUSY":
		return dialerapi.OutcomeBusy
	case "NO_ANSWER", "NO_USER_RESPONSE", "ALLOTTED_TIMEOUT":
		return dialerapi.OutcomeNoAnswer
	case "CALL_REJECTED",
		"NORMAL_TEMPORARY_FAILURE",
		"NORMAL_UNSPECIFIED",
		"NETWORK_OUT_OF_ORDER",
		"DESTINATION_OUT_OF_ORDER",
		"NO_ROUTE_DESTINATION",
		"NO_ROUTE_TRANSIT_NET",
		"INVALID_NUMBER_FORMAT",
		"FACILITY_REJECTED",
		"INCOMPATIBLE_DESTINATION",
		"REQUESTED_CHAN_UNAVAIL",
		"SWITCH_CONGESTION",
		"GATEWAY_DOWN",
		"PROTOCOL_ERROR",
		"MEDIA_TIMEOUT":
		return dialerapi.OutcomeTechFailure
	default:
		// Fail-safe: unknown causes route through tech_failure so they
		// cannot accidentally satisfy the success-class verify gate.
		return dialerapi.OutcomeTechFailure
	}
}

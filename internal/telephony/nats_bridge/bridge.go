// Package nats_bridge translates between NATS subjects and the FreeSWITCH ESL
// fleet. Inbound: tenant.<t>.telephony.cmd.<call_id> NATS messages dispatch
// to ESL commands via the pool, idempotency-checked through Redis SETNX 24h
// to survive publisher-side replays. Outbound: ESL events fan out from
// *pool.ESLPool.Events() onto per-call NATS subjects matching
// internal/telephony/api.SubjectChannelEventFor.
//
// Plan 11.1 Task 4 fills in the real bridge — Plan 09 Task 1 shipped the
// skeleton so cmd/telephony-bridge could compose a typed *Bridge into its
// graceful-shutdown sequence. The composition root upgrades from the raw
// *nats.Conn it used in the skeleton phase to pkg/eventbus.Publisher /
// Subscriber so the JetStream-vs-core-NATS choice lives behind a single
// abstraction and tests substitute fakes without spinning up nats-server.
package nats_bridge //nolint:revive // package name mirrors the module's filesystem path

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	telapi "github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/internal/telephony/esl"
	"github.com/sociopulse/platform/internal/telephony/pool"
	"github.com/sociopulse/platform/internal/telephony/router"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// Config holds the bridge's wiring. Field types are stable across plan
// tasks so the composition root does not churn between iterations.
type Config struct {
	// NATSPublisher is the outbound side: ESL events fan out here as
	// JSON-marshalled api.ChannelEvent payloads on per-call subjects.
	// Required.
	NATSPublisher eventbus.Publisher

	// NATSSubscriber is the inbound side: command envelopes are consumed
	// here under the "telephony-bridge" queue group on the
	// "tenant.*.telephony.cmd.>" wildcard. Required.
	NATSSubscriber eventbus.Subscriber

	// Pool is the ESL fleet the bridge dispatches commands to and reads
	// events from. Required.
	Pool *pool.ESLPool

	// Router is the trunk catalog. Reserved for future cmd-side
	// re-resolution (today's commands carry the resolved Node already);
	// kept on Config so Plan 11.x extensions can use it without churn.
	Router *router.Router

	// Redis stores idempotency keys (telephony:idempotency:<command_id>).
	// Required — running the bridge without idempotency would let
	// publisher-replay storms double-execute on the ESL fleet.
	Redis redis.UniversalClient

	// IdempotencyTTL is the dedup horizon. Zero falls back to 24h.
	IdempotencyTTL time.Duration

	// Metrics receives counter ticks. Nil-tolerated (observe* methods
	// are nil-safe).
	Metrics *Metrics

	// Logger is named for the bridge subsystem; nil-tolerated.
	Logger *zap.Logger
}

// Bridge is the composition-root handle. Owns the cmd subscriber + event
// publisher pair; Start spins them both up, Stop cancels the event-loop
// ctx and waits for the goroutine to exit. Drain is the graceful-shutdown
// alias the composition root calls before Stop.
type Bridge struct {
	cfg Config

	logger *zap.Logger

	cmdSub *cmdSubscriber
	evtPub *eventPublisher

	// cancelEvent cancels the ctx the event publisher loop reads —
	// captured at Start so Stop can fire it without the caller plumbing
	// the cancel func through their own shutdown choreography.
	cancelEvent context.CancelFunc
}

// New constructs a Bridge. Validates required deps; logger nil-safe.
// Returns error when Pool / NATSPublisher / NATSSubscriber / Redis are
// missing — those are wiring bugs the composition root should surface
// at boot rather than at first message.
func New(cfg Config) (*Bridge, error) {
	if cfg.Pool == nil {
		return nil, errors.New("nats_bridge: New: Pool required")
	}
	if cfg.NATSPublisher == nil {
		return nil, errors.New("nats_bridge: New: NATSPublisher required")
	}
	if cfg.NATSSubscriber == nil {
		return nil, errors.New("nats_bridge: New: NATSSubscriber required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("nats_bridge: New: Redis required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Bridge{cfg: cfg, logger: logger}, nil
}

// Start subscribes to inbound commands and spins up the event-publisher
// goroutine. Returns an error iff the cmd subscription registration
// fails — the event publisher itself never errors at Start (it only
// reads from a chan + a publisher).
//
// The event publisher's lifetime is bound by an internal child ctx so
// Stop can cancel it without affecting the caller's ctx.
func (b *Bridge) Start(ctx context.Context) error {
	guard := NewIdempotencyGuard(b.cfg.Redis, b.cfg.IdempotencyTTL, b.logger.Named("idempotency"))

	dispatcher := newPoolAdapter(b.cfg.Pool)

	b.cmdSub = newCmdSubscriber(b.cfg.NATSSubscriber, dispatcher, guard, b.cfg.Metrics, b.logger.Named("cmd_subscriber"))
	if err := b.cmdSub.Start(ctx); err != nil {
		return fmt.Errorf("nats_bridge: start cmd subscriber: %w", err)
	}

	// Detached ctx: the event-publisher loop owns its own lifetime so
	// Stop() (driven by Bridge.Drain or the deferred bridge.Stop in
	// cmd/telephony-bridge) can tear it down independently of the caller's
	// ctx. The cancel func is captured on the Bridge so Stop fires it.
	evtCtx, cancel := context.WithCancel(context.Background())
	b.cancelEvent = cancel

	b.evtPub = newEventPublisher(b.cfg.NATSPublisher, b.cfg.Pool.Events(), b.cfg.Metrics, b.logger.Named("event_publisher"))
	//nolint:contextcheck // detached ctx is intentional — see comment above
	b.evtPub.Run(evtCtx)

	b.logger.Info("nats_bridge started")
	return nil
}

// Stop cancels the event-publisher ctx and blocks until its goroutine
// exits. Idempotent — the underlying primitives gate double-calls.
//
// Inbound command subscriptions are NOT torn down here: the composition
// root is responsible for closing the eventbus.Subscriber, which triggers
// JetStream consumer drain.
func (b *Bridge) Stop() {
	if b.cancelEvent != nil {
		b.cancelEvent()
	}
	if b.evtPub != nil {
		b.evtPub.Stop()
	}
}

// Drain finishes in-flight messages and unsubscribes cleanly within the
// supplied context's deadline. Today this delegates to Stop — the
// underlying eventbus.Subscriber's drain is driven by Close (called by
// the composition root after Bridge.Drain).
//
// Returns nil when ctx hasn't expired by Stop completion; the
// composition root calls Drain before subscriber.Close so a SIGTERM
// gracefully completes commands that already started executing.
func (b *Bridge) Drain(_ context.Context) error {
	b.Stop()
	return nil
}

// poolAdapter wraps *pool.ESLPool and resolves each command into a per-
// node *esl.Client call. It satisfies poolDispatcher — the seam the
// cmd_subscriber consumes — so tests can substitute a fake without
// standing up the real ESL fleet. nil-pool guarded at Bridge.New.
type poolAdapter struct {
	pool *pool.ESLPool
}

func newPoolAdapter(p *pool.ESLPool) *poolAdapter {
	return &poolAdapter{pool: p}
}

// Originate translates the api.OriginateCommand DTO into the internal
// esl.OriginateRequest shape and invokes Originate on the resolved
// client. The translation pulls the SIP gateway destination from
// (TrunkID, Number); recording path + caller-id propagate via FS channel
// variables.
func (a *poolAdapter) Originate(ctx context.Context, node string, cmd telapi.OriginateCommand) error {
	cli, err := a.pool.Get(node)
	if err != nil {
		return fmt.Errorf("nats_bridge: pool.Get %q: %w", node, err)
	}
	req := buildOriginateRequest(cmd)
	if _, err := cli.Originate(ctx, req); err != nil {
		return fmt.Errorf("nats_bridge: esl originate: %w", err)
	}
	return nil
}

// buildOriginateRequest maps the cross-module api.OriginateCommand DTO to
// the package-local esl.OriginateRequest. Pulled out so the mapping
// sits in one place and the test surface is small.
//
// Channel-variable names match mod_dialplan_xml expectations. Empty
// fields are skipped so we don't push noisy "" values onto the FS
// originate string.
func buildOriginateRequest(cmd telapi.OriginateCommand) esl.OriginateRequest {
	vars := map[string]string{
		"sociopulse_call_id":   cmd.CallID.String(),
		"sociopulse_tenant_id": cmd.TenantID.String(),
		"sociopulse_command":   cmd.CommandID.String(),
	}
	if cmd.RecordingPath != "" {
		vars["recording_path"] = cmd.RecordingPath
	}
	if cmd.OperatorExt != "" {
		vars["operator_ext"] = cmd.OperatorExt
	}

	callURL := buildCallURL(cmd.TrunkID, cmd.Number)

	return esl.OriginateRequest{
		CallURL:    callURL,
		Caller:     cmd.CallerID,
		Variables:  vars,
		Timeout:    cmd.DialingTimeout,
		Extension:  "",           // default to &park()
		CallerName: cmd.CallerID, // mirror caller id name = number for now
	}
}

// buildCallURL is the FS originate URL for a (trunk, number) pair. We
// emit the canonical sofia/gateway/<trunk>/<number> shape — the same
// form Plan 09 router strategies test against.
func buildCallURL(trunkID, number string) string {
	return strings.Join([]string{"sofia/gateway/", trunkID, "/", number}, "")
}

// Hangup terminates a call by callID via uuid_kill on the resolved node.
// cause defaults inside esl.Hangup when empty.
func (a *poolAdapter) Hangup(ctx context.Context, node, callID, cause string) error {
	cli, err := a.pool.Get(node)
	if err != nil {
		return fmt.Errorf("nats_bridge: pool.Get %q: %w", node, err)
	}
	if err := cli.Hangup(ctx, callID, cause); err != nil {
		return fmt.Errorf("nats_bridge: esl hangup: %w", err)
	}
	return nil
}

// MixMonitorStart begins recording the call to path on the resolved node.
// flags is intentionally nil — Plan 11.1 keeps the bridge command shape
// minimal; per-tenant recording flag policy lives upstream (Plan 12).
func (a *poolAdapter) MixMonitorStart(ctx context.Context, node, callID, path string) error {
	cli, err := a.pool.Get(node)
	if err != nil {
		return fmt.Errorf("nats_bridge: pool.Get %q: %w", node, err)
	}
	if err := cli.MixMonitorStart(ctx, callID, path, nil); err != nil {
		return fmt.Errorf("nats_bridge: esl mixmonitor start: %w", err)
	}
	return nil
}

// MixMonitorStop stops every recording on the call. Per the Plan 11.1
// envelope shape, the stop command carries no path argument — we map
// "stop all" via the FS-supported wildcard "all" so a single command
// drops every active recording on the channel without the bridge having
// to track per-recording paths.
func (a *poolAdapter) MixMonitorStop(ctx context.Context, node, callID string) error {
	cli, err := a.pool.Get(node)
	if err != nil {
		return fmt.Errorf("nats_bridge: pool.Get %q: %w", node, err)
	}
	if err := cli.MixMonitorStop(ctx, callID, "all"); err != nil {
		return fmt.Errorf("nats_bridge: esl mixmonitor stop: %w", err)
	}
	return nil
}

// Compile-time check: *poolAdapter satisfies poolDispatcher. Catches
// signature drift if either side moves before the other.
var _ poolDispatcher = (*poolAdapter)(nil)

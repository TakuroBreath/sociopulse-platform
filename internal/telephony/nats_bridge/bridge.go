// Package nats_bridge translates between NATS subjects and the FreeSWITCH ESL
// fleet. Inbound: tenant.<t>.telephony.cmd.<call_id> NATS messages dispatch
// to ESL commands via the pool. Outbound: ESL events produce per-call NATS
// subjects matching internal/telephony/api.SubjectChannelEventFor.
//
// TODO(plan-09-task-2-3): Plan 09 Task 2 (ESL client) and Task 3 (idempotency
// + command dispatch) fill in the real bridge. This skeleton exists only so
// cmd/telephony-bridge can compose a typed *Bridge and wire it into its
// graceful-shutdown sequence.
//
// Why a skeleton ships in Task 1: the composition root must know how to
// .Start the bridge before /readyz can return 200. Shipping the boundary
// first lets Tasks 2/3 land their bodies behind the same surface.
package nats_bridge //nolint:revive // package name mirrors the module's filesystem path

import (
	"context"

	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/telephony/pool"
	"github.com/sociopulse/platform/internal/telephony/router"
)

// Config holds the bridge's wiring. Field names are stable across tasks so
// the composition root does not churn between plan implementations.
type Config struct {
	// NATS is the connection the bridge subscribes / publishes against.
	// Tests can pass a connection produced by an embedded test server.
	NATS *nats.Conn

	// Pool is the ESL fleet the bridge dispatches commands to.
	Pool *pool.ESLPool

	// Router resolves {fs_node, trunk} for outbound originates.
	Router *router.Router

	// Redis stores idempotency keys (op:idempotency:<command_id>) and the
	// per-call backpressure counters used by Router.
	Redis *redis.Client

	// Logger is named for the bridge subsystem; nil-tolerated.
	Logger *zap.Logger
}

// Bridge is the composition-root handle. Real subscription state moves here
// in Task 2-3.
type Bridge struct {
	logger *zap.Logger
}

// New constructs a Bridge skeleton. Cannot fail today — validation moves here
// in Task 2.
func New(cfg Config) *Bridge {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Bridge{logger: logger}
}

// Start subscribes to NATS subjects and begins translating to ESL.
// Skeleton no-ops; the contract is "Start must be safe to call once and must
// not block".
func (b *Bridge) Start(_ context.Context) error {
	return nil
}

// Stop closes subscriptions and stops dispatching. Skeleton no-ops.
func (b *Bridge) Stop() {
	// no subscriptions to drain yet
}

// Drain finishes in-flight messages and unsubscribes cleanly within the
// supplied context's deadline. Skeleton no-ops.
//
// The composition root calls Drain before Stop so that a SIGTERM gracefully
// completes commands that already started executing — Task 3 fills in the
// timeout-aware cancellation path.
func (b *Bridge) Drain(_ context.Context) error {
	return nil
}

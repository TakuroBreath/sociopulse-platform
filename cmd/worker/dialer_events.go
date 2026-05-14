package main

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer"
	"github.com/sociopulse/platform/internal/dialer/fsm"
	dialertnats "github.com/sociopulse/platform/internal/dialer/transport/nats"
	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// callEventBoot bundles the dialer call-event subscriber's lifecycle-
// owned dependencies. cmd/worker constructs one of these and defers
// Close on it; the subscriber itself runs on the eventbus push
// dispatcher goroutine so there is no explicit Run loop to join.
//
// fsm is held only to keep the FSM machine alive for the subscription's
// lifetime; cmd/worker has no other consumer of it today (the retry
// orchestrator builds its own collaborators). Closing it is a no-op
// — fsm.Machine has no graceful-shutdown surface beyond ctx cancel.
//
// The bus's Close (deferred separately in main.go) is what tears down
// the subscription. callEventBoot.Close exists so the caller can
// `defer boot.Close(logger)` symmetric with analyticsBoot.Close
// regardless of whether the boot path succeeded.
type callEventBoot struct {
	subscriber *dialertnats.CallEventSubscriber
	fsm        *fsm.Machine
}

// Close is a no-op today (kept for lifecycle symmetry). The push
// subscription is owned by the bus (closed on shutdown by the outer
// defer in run()); the fsm.Machine has no internal goroutines that
// need draining.
//
// Nil-receiver tolerant: a degraded boot returns (nil, nil) so the
// `defer boot.Close(logger)` at the call site is safe even when the
// subscriber failed to wire.
//
//nolint:unparam // logger reserved for future drain diagnostics
func (b *callEventBoot) Close(_ *zap.Logger) {
	if b == nil {
		return
	}
}

// buildCallEventSubscriber wires the dialer's telephony-event consumer
// for cmd/worker. Returns nil + nil when the pre-conditions for wiring
// are not met (degraded boot): missing NATS subscriber, missing redis
// (the worker boots redis-less for the smoke test path), or any
// constructor failure. Returns a non-nil error only on a configuration
// mistake the operator must fix.
//
// Why cmd/worker has its OWN FSM rather than reaching into cmd/api:
// the FSM is per-process state on top of a SHARED Redis store. cmd/api
// owns the operator-HTTP-driven transitions; cmd/worker owns the
// telephony-event-driven transitions. Both write to the same Redis
// hashes under optimistic-concurrency CAS, so two writers are safe.
// The shared JetStream queue group "dialer-call-events" (set by the
// subscriber's DefaultQueueGroup) ensures each telephony event is
// processed by exactly one replica — load-balanced across the cmd/api
// + cmd/worker pods.
//
// Publisher (snapshot fan-out) is deliberately nil: a transition
// triggered in cmd/worker writes the durable state to Redis, but the
// snapshot does not fan out to WS clients from this process. WS
// clients connected via cmd/api still see the next OPERATOR-INITIATED
// transition's snapshot, and the operator UI's call-state poll loop
// covers the gap. A proper cross-replica Snapshot bus would be a
// follow-up (introduce dialer.NATSPubSub here mirroring cmd/api).
func buildCallEventSubscriber(
	cfg dialerEventConfig,
) (*callEventBoot, error) {
	if cfg.bus == nil {
		cfg.logger.Warn("dialer call-event subscriber skipped: NATS subscriber unavailable")
		return nil, nil
	}
	if cfg.pool == nil {
		cfg.logger.Warn("dialer call-event subscriber skipped: postgres pool unavailable")
		return nil, nil
	}
	if cfg.redis == nil {
		cfg.logger.Warn("dialer call-event subscriber skipped: redis unavailable")
		return nil, nil
	}

	machine, err := fsm.New(fsm.Config{
		Redis:  cfg.redis,
		PG:     cfg.pool,
		Outbox: outbox.NewPostgresWriter(),
		Logger: cfg.logger.Named("fsm"),
		// Publisher intentionally nil — see the function doc.
	})
	if err != nil {
		return nil, fmt.Errorf("cmd/worker: build dialer FSM: %w", err)
	}

	lookup := dialer.NewPgCallOperatorLookup(cfg.pool)
	subscriber := dialertnats.NewCallEventSubscriber(machine, lookup, cfg.logger.Named("call_event_subscriber"))

	// Subscribe under the shared queue group so cmd/api's
	// Module.Register-side subscription (when present) and our
	// subscription share message delivery. JetStream handles the
	// load-balancing.
	if err := subscriber.Subscribe(cfg.ctx, cfg.bus, dialertnats.DefaultQueueGroup); err != nil {
		return nil, fmt.Errorf("cmd/worker: subscribe call events: %w", err)
	}

	cfg.logger.Info("dialer call-event subscriber registered",
		zap.String("subject", dialertnats.SubscribeSubject),
		zap.String("queue", dialertnats.DefaultQueueGroup),
	)
	return &callEventBoot{subscriber: subscriber, fsm: machine}, nil
}

// dialerEventConfig bundles the buildCallEventSubscriber inputs so the
// call site stays terse. ctx is honoured during the Subscribe
// consumer-create RPC only; runtime delivery is owned by the bus.
type dialerEventConfig struct {
	ctx    context.Context
	bus    eventbus.Subscriber
	pool   *postgres.Pool
	redis  *redis.Client
	logger *zap.Logger
}

// asRedisClientForFSM mirrors internal/dialer.module.go::asRedisClient
// but tolerates a nil input (the worker's openRedis can return nil on
// degraded boot). Returns nil when the type assertion fails.
func asRedisClientForFSM(uc redis.UniversalClient) *redis.Client {
	if uc == nil {
		return nil
	}
	rdb, _ := uc.(*redis.Client)
	return rdb
}

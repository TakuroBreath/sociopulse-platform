// Package dialer — Module registration entry point + composition root.
//
// Plan 10 Task 10 wires the full dialer:
//
//  1. *fsm.Machine (operator FSM) backed by Postgres + Redis + outbox.
//  2. *queue.RedisQueue (per-(tenant, project) ZSET).
//  3. *router.Router (telephony bridge adapter, with stubs for the
//     missing locator entries until Plan 11 wires the cluster bridge
//     consumer).
//  4. *capacity.Tracker (per-FS-node 60-channel cap) — uses stub
//     pool/backpressure when the cmd/api boot has no real telephony
//     module wired (today's reality; Plan 11 swaps in the bridge).
//  5. *hours.Checker (per-tenant + per-region working-hours gate)
//     adapted over tenancy.SettingsCache (with a noop fallback when
//     tenancy is missing — useful for the worker-only boot).
//  6. *rdd.Generator (Random Digit Dialing).
//  7. *retry.Orchestrator — built but NOT started here. cmd/worker
//     fetches it from the locator and runs Run; cmd/api skips Run so
//     the orchestrator only spins up in the worker process.
//  8. PubSub — in-memory per-(tenant, operator) Snapshot fan-out wired
//     into the FSM's Publisher hook so every successful transition
//     reaches the WS handler.
//  9. Heartbeat watchdog — goroutine that scans op:*:user:* keys every
//     30s and forces operators offline when their presence:<t>:user:<o>
//     key has expired (Task 2c).
//  10. HTTP transport mounted on d.HTTPRouter when present.
//  11. Locator registration of every dialer service so cross-module
//     callers can resolve them.
//
// Required Deps:
//
//	d.Logger        — non-nil
//	d.Pool          — non-nil (Postgres pool)
//	d.Redis         — non-nil (Redis client)
//	d.Locator       — non-nil
//
// Optional Deps:
//
//	d.HTTPRouter    — when non-nil, /api/sessions, /api/calls, and
//	                  /api/operator/* are mounted.
//	d.EventBus      — currently unused; Plan 11 wires the NATS-backed
//	                  Snapshot fan-out via this slot.
//
// Optional Locator entries (registered earlier by other modules):
//
//	tenancy.Tenancy            — for the SettingsCache adapter; missing
//	                              → noopSettingsLookup (platform default
//	                              hours for every tenant).
//	tenancy.KMSResolver        — for the retry decryptor adapter; missing
//	                              → passthroughDecryptor (dev/test only).
//	telephony.CommandPublisher — for the dialer router; missing → router
//	                              not constructed (HTTP transport
//	                              degrades; FSM still runs).
//	auth.RBACChecker, auth.ClaimsValidator — required for HTTP mount;
//	                              missing → HTTP transport not mounted.
//	audit.Logger               — falls back to noop logger.
package dialer

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/capacity"
	"github.com/sociopulse/platform/internal/dialer/fsm"
	"github.com/sociopulse/platform/internal/dialer/hours"
	"github.com/sociopulse/platform/internal/dialer/queue"
	"github.com/sociopulse/platform/internal/dialer/rdd"
	"github.com/sociopulse/platform/internal/dialer/retry"
	"github.com/sociopulse/platform/internal/dialer/router"
	transporthttp "github.com/sociopulse/platform/internal/dialer/transport/http"
	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/regions"
)

// Locator keys this module registers. External modules look these up
// to obtain the dialer interfaces without crossing into internal/dialer/{fsm,queue,...}.
const (
	LocatorOperatorFSM         = "dialer.OperatorFSM"
	LocatorCallQueue           = "dialer.CallQueue"
	LocatorRouter              = "dialer.Router"
	LocatorLineCapacityTracker = "dialer.LineCapacityTracker"
	LocatorWorkingHoursChecker = "dialer.WorkingHoursChecker"
	LocatorRDDGenerator        = "dialer.RDDGenerator"
	LocatorRetryOrchestrator   = "dialer.RetryOrchestrator"
	LocatorSnapshotPubSub      = "dialer.SnapshotPubSub"
)

// Locator keys this module CONSUMES (registered by other modules).
const (
	locatorRBACChecker     = "auth.RBACChecker"
	locatorClaimsValidator = "auth.ClaimsValidator"
	// Note: locatorTenancy / locatorKMSResolver / locatorCommandPublisher
	// live in adapters.go.
)

// pubsubLifecycle is the unified Stop() seam shared by both PubSub
// backends. Plan 11.1 Task 3 introduced *NATSPubSub which exposes
// Stop() error; the existing in-memory *PubSub keeps Close() with no
// error return. Module.Stop calls a single method through this seam,
// so a future third backend just needs to satisfy the interface
// without re-plumbing the lifecycle code.
type pubsubLifecycle interface {
	Stop() error
}

// inMemPubSubAdapter wraps the legacy in-memory PubSub.Close() so it
// satisfies pubsubLifecycle. Returning a typed nil keeps Module.Stop's
// error path identical for both backends.
type inMemPubSubAdapter struct{ *PubSub }

// Stop closes the underlying in-memory PubSub. Always returns nil —
// Close has no failure mode (it just closes channels and flips a flag).
func (a inMemPubSubAdapter) Stop() error {
	a.PubSub.Close()
	return nil
}

// Module is the top-level registration handle for the dialer module.
// Holds the lifecycle-owned components built in Register; Stop()
// releases them. Safe to construct as a zero value.
type Module struct {
	// mu guards lifecycle bookkeeping. Module is stateless apart from
	// the long-lived components below.
	mu sync.Mutex

	// Lifecycle-owned components — Stop() shuts them down.
	//
	// pubsub holds whichever Snapshot fan-out backend Register chose:
	// a NATS-backed *NATSPubSub when (Deps.EventBus, Deps.Subscriber)
	// are present, otherwise the in-memory *PubSub wrapped in the
	// inMemPubSubAdapter (for Redis-less / NATS-less test setups).
	pubsub        pubsubLifecycle
	heartbeatStop func()
	heartbeatWG   sync.WaitGroup
	stopped       bool

	// replicaID identifies this pod in the dialer-pubsub queue group
	// and pairs with the realtime module's replicaID-named scheme so
	// observability (consumer lists, presence map) line up across
	// modules. Default is uuid.NewString() at first Register.
	replicaID string

	// Built but not started here — cmd/worker calls Run on this. The
	// field is exposed via the locator under LocatorRetryOrchestrator.
	retryOrch *retry.Orchestrator

	// Logger / metrics retained for shutdown diagnostics.
	logger *zap.Logger
}

// Compile-time assertion that *Module satisfies the modules.Module
// contract. Mirrors the pattern used by tenancy / auth.
var _ modules.Module = (*Module)(nil)

// Name returns the module's unique identifier within the registry.
func (*Module) Name() string { return "dialer" }

// Register wires the module's components into the composition root.
// See the package-level comment for the full sequence.
//
// Register is intentionally linear and resilient: missing optional
// deps log a warn and degrade gracefully rather than fatal — cmd/api
// must boot in test environments where (e.g.) the tenancy module
// hasn't been wired yet.
//
//nolint:gocognit,gocyclo,cyclop // composition is intentionally linear
func (m *Module) Register(d modules.Deps) error {
	if err := requireDeps(d); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	logger := d.Logger.Named("dialer")
	m.logger = logger

	// 1. Regions snapshot. Loaded once per process; the embedded YAML
	//    means this is filesystem-free.
	regs, err := regions.Load()
	if err != nil {
		return fmt.Errorf("dialer: load regions: %w", err)
	}

	// 2. PubSub — built BEFORE the FSM so the FSM can see it via
	//    Publisher. Plan 11.1 Task 3 added the NATS-backed adapter:
	//    when (Deps.EventBus, Deps.Subscriber) are both present we
	//    wire NATSPubSub so a snapshot published on pod A reaches WS
	//    subscribers on pod B (cross-replica fan-out). Otherwise we
	//    fall back to the in-memory PubSub for Redis-less / NATS-less
	//    test setups — keeping the existing dialer test suite green
	//    without forcing every module test to embed JetStream.
	if m.replicaID == "" {
		m.replicaID = uuid.NewString()
	}
	var (
		fsmPublisher   Publisher
		snapshotPubsub transporthttp.SnapshotPubSub
	)
	if d.EventBus != nil && d.Subscriber != nil {
		natsPS := NewNATSPubSub(d.EventBus, d.Subscriber, m.replicaID, logger.Named("dialer.pubsub"))
		startCtx := d.Ctx
		if startCtx == nil {
			startCtx = context.Background()
		}
		if err := natsPS.Start(startCtx); err != nil {
			return fmt.Errorf("dialer: start nats pubsub: %w", err)
		}
		m.pubsub = natsPS
		fsmPublisher = natsPS
		snapshotPubsub = natsPS
	} else {
		inMem := NewPubSub()
		m.pubsub = inMemPubSubAdapter{inMem}
		fsmPublisher = inMem
		snapshotPubsub = inMem
	}

	// 3. FSM Machine. d.Redis is the broader UniversalClient; the FSM
	//    needs *redis.Client (single-node connection for Lua scripts).
	//    requireDeps() above already verified the assertion succeeds.
	rdb := asRedisClient(d.Redis)
	machine, err := fsm.New(fsm.Config{
		Redis:     rdb,
		PG:        d.Pool,
		Outbox:    outbox.NewPostgresWriter(),
		Logger:    logger.Named("fsm"),
		Publisher: fsmPublisher,
	})
	if err != nil {
		return fmt.Errorf("dialer: build fsm: %w", err)
	}

	// 4. Queue.
	q, err := queue.New(queue.Config{
		Redis:  rdb,
		Logger: logger.Named("queue"),
	})
	if err != nil {
		return fmt.Errorf("dialer: build queue: %w", err)
	}

	// 5. Router. Requires telephony.CommandPublisher; bails (and warns)
	//    when missing rather than panic so a worker-only boot still
	//    proceeds.
	var dialerRouter *router.Router
	publisher, ok := lookupCommandPublisher(d.Locator, logger)
	if ok {
		consumer := &stubEventConsumer{logger: logger.Named("router")}
		dialerRouter, err = router.New(router.Config{
			Publisher: publisher,
			Consumer:  consumer,
			Logger:    logger.Named("router"),
		})
		if err != nil {
			logger.Warn("dialer: router construction failed; HTTP transport degraded",
				zap.Error(err))
			dialerRouter = nil
		}
	} else {
		logger.Warn("telephony.CommandPublisher missing from locator — dialer router disabled")
	}

	// 6. Capacity tracker. cmd/api today has no real Pool /
	//    Backpressure (those live in cmd/telephony-bridge); we wire
	//    stubs so the construction succeeds and the dialer surfaces
	//    ErrAllNodesFull on every Acquire — the right behaviour when
	//    the bridge isn't running.
	tracker, err := capacity.New(capacity.Config{
		Pool:         stubCapacityPool{},
		Backpressure: stubBackpressure{},
		Logger:       logger.Named("capacity"),
	})
	if err != nil {
		return fmt.Errorf("dialer: build capacity tracker: %w", err)
	}

	// 7. WorkingHoursChecker. Adapter over tenancy.SettingsCache with a
	//    noop fallback when tenancy isn't in the locator.
	var settingsLookup hours.SettingsLookup = noopSettingsLookup{}
	if t := lookupTenancy(d.Locator, logger); t != nil {
		settingsLookup = &settingsLookupAdapter{cache: t}
	} else {
		logger.Info("tenancy.Tenancy missing from locator — using noop settings lookup (platform default working hours)")
	}
	checker, err := hours.New(hours.Config{
		Settings: settingsLookup,
		Regions:  regs,
		Logger:   logger.Named("hours"),
	})
	if err != nil {
		return fmt.Errorf("dialer: build hours checker: %w", err)
	}

	// 8. RDD Generator. Plan 11 wires the actual crm RespondentService;
	//    today we look up the LocatorRespondentService key and skip RDD
	//    construction when missing — RDD is a generator-only feature
	//    and not on the FSM critical path.
	var rddGen *rdd.Generator
	if rs := lookupCRMRespondentService(d.Locator, logger); rs != nil {
		rddGen, err = rdd.New(rdd.Config{
			Redis:   rdb,
			Queue:   q,
			Crm:     rs,
			Regions: regs,
			Logger:  logger.Named("rdd"),
			Rand:    newRDDRand(),
		})
		if err != nil {
			logger.Warn("dialer: RDD generator construction failed", zap.Error(err))
			rddGen = nil
		}
	} else {
		logger.Info("crm.RespondentService missing from locator — RDD generator disabled")
	}

	// 9. Retry Orchestrator (built but not run here). Uses the
	//    KMSResolver-adapted decryptor when tenancy is wired; falls
	//    back to a passthrough decryptor in test / pre-Plan-04 boots.
	var decryptor retry.Decryptor = passthroughDecryptor{}
	if r := lookupKMSResolver(d.Locator, logger); r != nil {
		decryptor = &kmsDecryptorAdapter{kms: r}
	} else {
		logger.Warn("tenancy.KMSResolver missing from locator — retry orchestrator using passthrough decryptor (dev/test only)")
	}
	leader, err := retry.NewPgLeader(d.Pool, 0, logger.Named("retry.leader"))
	if err != nil {
		return fmt.Errorf("dialer: build retry leader: %w", err)
	}
	reader, err := retry.NewPgReader(d.Pool)
	if err != nil {
		return fmt.Errorf("dialer: build retry reader: %w", err)
	}
	retryOrch, err := retry.New(retry.Config{
		Leader:    leader,
		Reader:    reader,
		Decryptor: decryptor,
		Queue:     q,
		Logger:    logger.Named("retry"),
	})
	if err != nil {
		return fmt.Errorf("dialer: build retry orchestrator: %w", err)
	}
	m.retryOrch = retryOrch

	// 10. Heartbeat watchdog. Started immediately as a child goroutine
	//     of the supplied d.Ctx so a Stop() (or a parent ctx cancel)
	//     drains it cleanly. Goroutine leaks are caught by goleak in
	//     pubsub_test.go.
	hb, err := fsm.NewHeartbeat(fsm.HeartbeatConfig{
		Redis:  rdb,
		FSM:    machine,
		Logger: logger.Named("heartbeat"),
	})
	if err != nil {
		return fmt.Errorf("dialer: build heartbeat: %w", err)
	}
	parent := d.Ctx
	if parent == nil {
		parent = context.Background()
	}
	hbCtx, hbCancel := context.WithCancel(parent)
	m.heartbeatStop = hbCancel
	// Go 1.25 wg.Go — replaces wg.Add(1); go func(){ defer wg.Done(); ... }()
	// per Plan 09 carry-forward checklist #11.
	m.heartbeatWG.Go(func() {
		_ = hb.Run(hbCtx)
	})

	// 11. HTTP transport. Mount only when an HTTPRouter is available
	//     (cmd/api wires it; cmd/worker does not) and the auth deps
	//     are in the locator. The router (Step 5) is also required —
	//     skip the mount when it failed to construct.
	if d.HTTPRouter != nil {
		if dialerRouter == nil {
			logger.Warn("dialer: HTTP transport not mounted (router missing)")
		} else if err := m.mountHTTP(d, machine, dialerRouter, q, checker, tracker, rdb, snapshotPubsub); err != nil {
			logger.Warn("dialer: HTTP transport not mounted", zap.Error(err))
		}
	} else {
		logger.Debug("d.HTTPRouter is nil — skipping dialer HTTP transport mount")
	}

	// 12. Locator registrations.
	d.Locator.Register(LocatorOperatorFSM, dialerapi.OperatorFSM(machine))
	d.Locator.Register(LocatorCallQueue, dialerapi.CallQueue(q))
	if dialerRouter != nil {
		d.Locator.Register(LocatorRouter, dialerapi.Router(dialerRouter))
	}
	d.Locator.Register(LocatorLineCapacityTracker, dialerapi.LineCapacityTracker(tracker))
	d.Locator.Register(LocatorWorkingHoursChecker, dialerapi.WorkingHoursChecker(checker))
	if rddGen != nil {
		d.Locator.Register(LocatorRDDGenerator, dialerapi.RDDGenerator(rddGen))
	}
	d.Locator.Register(LocatorRetryOrchestrator, dialerapi.RetryOrchestrator(retryOrch))
	d.Locator.Register(LocatorSnapshotPubSub, snapshotPubsub)

	logger.Info("dialer module registered (Plan 10 Task 10)",
		zap.Bool("http_mounted", d.HTTPRouter != nil && dialerRouter != nil),
		zap.Bool("router_wired", dialerRouter != nil),
		zap.Bool("rdd_wired", rddGen != nil),
	)
	return nil
}

// Stop releases the module's lifecycle-owned components: cancels the
// heartbeat watchdog, waits for it to drain, and closes the PubSub
// (whichever backend Register chose). Safe to call multiple times —
// the second invocation is a no-op for the module shell, and each
// backend's Stop is itself idempotent.
func (m *Module) Stop() error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopped = true
	hbStop := m.heartbeatStop
	pubsub := m.pubsub
	m.mu.Unlock()

	if hbStop != nil {
		hbStop()
	}
	m.heartbeatWG.Wait()
	if pubsub != nil {
		// Both backends' Stop is idempotent + nil-safe; we surface the
		// error for the NATSPubSub variant (currently always nil but
		// reserved for future drain failures) but do NOT abort: the
		// heartbeat goroutine has already wound down and there's no
		// recovery action a caller could take.
		if err := pubsub.Stop(); err != nil && m.logger != nil {
			m.logger.Warn("dialer: pubsub stop returned error", zap.Error(err))
		}
	}
	if m.logger != nil {
		m.logger.Info("dialer module stopped")
	}
	return nil
}

// mountHTTP composes the HTTP transport's Deps and mounts the
// handlers under /api. Auth deps come from the locator (registered
// earlier by the auth module); when missing we return a clean error
// so the caller surfaces a warning rather than panicking.
//
// snapshotPubsub is whichever backend Register selected (NATSPubSub
// when the bus is wired, in-memory PubSub otherwise) — passed in
// rather than re-derived from m.pubsub so this function works
// against the lifecycle interface without an extra type assertion.
func (m *Module) mountHTTP(
	d modules.Deps,
	machine *fsm.Machine,
	router *router.Router,
	q *queue.RedisQueue,
	checker *hours.Checker,
	tracker *capacity.Tracker,
	rdb *redis.Client,
	snapshotPubsub transporthttp.SnapshotPubSub,
) error {
	rbac, ok := lookupRBACChecker(d.Locator, m.logger)
	if !ok {
		return errors.New("auth.RBACChecker missing from locator")
	}
	validator, ok := lookupClaimsValidator(d.Locator, m.logger)
	if !ok {
		return errors.New("auth.ClaimsValidator missing from locator")
	}

	// Heartbeat presence-refresh adapter — fired by the transport
	// middleware on every authenticated request. Passing ttl=0 lets
	// fsm.RefreshPresence apply its own default (defaultHeartbeatPresenceTTL,
	// which pairs with the watchdog's sweep cadence). Locking the TTL
	// here would couple this composition root to a watchdog tunable that
	// should stay owned by the fsm package.
	var refresh transporthttp.RefreshFn
	if rdb != nil {
		refresh = func(ctx context.Context, tenantID, operatorID uuid.UUID) error {
			return fsm.RefreshPresence(ctx, rdb, tenantID, operatorID, 0)
		}
	}

	transporthttp.Mount(d.HTTPRouter.Group("/api"), transporthttp.Deps{
		FSM:             machine,
		Router:          router,
		Queue:           q,
		Hours:           checker,
		Capacity:        tracker,
		Validator:       validator,
		RBAC:            rbac,
		Logger:          m.logger.Named("http"),
		SnapshotPubSub:  snapshotPubsub,
		RefreshPresence: refresh,
	})
	m.logger.Info("dialer HTTP transport mounted under /api")
	return nil
}

// requireDeps validates that every Register prerequisite is non-nil.
// Returning a structured error (rather than panicking) lets cmd/api
// surface a clean message at boot.
func requireDeps(d modules.Deps) error {
	switch {
	case d.Logger == nil:
		return errors.New("dialer: Deps.Logger is required")
	case d.Pool == nil:
		return errors.New("dialer: Deps.Pool is required")
	case d.Redis == nil:
		return errors.New("dialer: Deps.Redis is required")
	case d.Locator == nil:
		return errors.New("dialer: Deps.Locator is required")
	}
	// FSM, queue, and the heartbeat watchdog all need a *redis.Client
	// (not the broader UniversalClient interface), so reject upfront
	// if d.Redis is wrapped behind a Cluster/Sentinel client.
	if _, ok := d.Redis.(*redis.Client); !ok {
		return fmt.Errorf("dialer: Deps.Redis must be a *redis.Client (got %T)", d.Redis)
	}
	return nil
}

// asRedisClient extracts the concrete *redis.Client from the
// UniversalClient. requireDeps() above is the type-check; this
// helper is the cast. Returns nil when the type assertion fails —
// callers reach this only after requireDeps() succeeded.
func asRedisClient(uc redis.UniversalClient) *redis.Client {
	rdb, _ := uc.(*redis.Client)
	return rdb
}

// lookupRBACChecker pulls auth.RBACChecker out of the locator. Mirrors
// the pattern in crm/surveys; returns ok=false on missing or
// type-mismatched so the caller surfaces a clean warning.
func lookupRBACChecker(loc modules.ServiceLocator, log *zap.Logger) (authapi.RBACChecker, bool) {
	raw, ok := loc.Lookup(locatorRBACChecker)
	if !ok {
		return nil, false
	}
	c, ok := raw.(authapi.RBACChecker)
	if !ok {
		log.Error("auth.RBACChecker registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil, false
	}
	return c, true
}

// lookupClaimsValidator pulls auth.ClaimsValidator out of the locator.
func lookupClaimsValidator(loc modules.ServiceLocator, log *zap.Logger) (authapi.ClaimsValidator, bool) {
	raw, ok := loc.Lookup(locatorClaimsValidator)
	if !ok {
		return nil, false
	}
	v, ok := raw.(authapi.ClaimsValidator)
	if !ok {
		log.Error("auth.ClaimsValidator registered with wrong type",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil, false
	}
	return v, true
}

// lookupCRMRespondentService pulls crm.RespondentService out of the
// locator without forcing the dialer module to import the crm
// package. We use the rdd.CRM small-surface interface; the crm
// service implements it (Create method), so the locator-stored
// crm.RespondentService satisfies rdd.CRM.
//
// Returns nil when missing or wrong-typed; the caller skips RDD
// generator construction.
func lookupCRMRespondentService(loc modules.ServiceLocator, log *zap.Logger) rdd.CRM {
	raw, ok := loc.Lookup("crm.RespondentService")
	if !ok {
		return nil
	}
	c, ok := raw.(rdd.CRM)
	if !ok {
		log.Error("crm.RespondentService registered without rdd.CRM surface",
			zap.String("got_type", fmt.Sprintf("%T", raw)))
		return nil
	}
	return c
}

// newRDDRand returns a freshly seeded *rand.ChaCha8 source for RDD's
// random prefix / subscriber rolls. Deterministic test seeding is
// done by overriding Config.Rand directly.
func newRDDRand() *rand.ChaCha8 {
	var seed [32]byte
	// Best effort: seed from the system clock + a per-call ticker.
	// rdd.New itself uses crypto/rand when Rand is nil, so this is
	// only the fallback for cases where we want a non-nil but
	// deterministic-style seed at boot. We keep it simple — the rdd
	// package handles the higher-quality entropy source internally.
	now := time.Now().UnixNano()
	for i := range seed {
		seed[i] = byte(now >> (i % 8))
	}
	return rand.NewChaCha8(seed)
}

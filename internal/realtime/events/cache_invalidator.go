// cache_invalidator.go subscribes to module lifecycle events and
// invalidates the realtime resolver caches so a project archive,
// user delete, or recording delete doesn't leave 60s of stale-cached
// cross-tenant approvals (the cache's lazy-expiry default in
// service/resolver_cache.go).
//
// Subscriptions (Plan 11.4 Task 6):
//
//   - tenant.*.crm.project.status_changed (Plan 11.3 Task 3 — required)
//     payload: crmapi.ProjectStatusChangedEvent{ProjectID, TenantID, ...}
//   - tenant.*.auth.user.deleted (optional — skipped if UserInvalidate nil)
//     payload: authapi.UserDeletedEvent{UserID, TenantID, ...}
//   - tenant.*.recording.call.deleted (optional — skipped if CallInvalidate nil)
//     payload: rapi.RecordingCallDeletedEvent{RecordingID, CallID, TenantID, ...}
//
// Each per-message handler unmarshals its specific event type, ticks
// the realtime_cache_invalidations_total{subject, result} counter on
// every dispatch outcome (ok / parse_error / empty_id), and forwards
// the resolved entity ID to the matching invalidator callback.
//
// Carry-forward of the events package patterns from Plan 11
// Task 4b (NATSSubscriber) and Plan 11.1 Task 2 (TrunksReplicator):
// one Subscribe per subject, one handler, metric tick on every dispatch
// outcome, no goroutines beyond the bus's push consumer.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// entityInvalidateFn is the narrow callback signature *CacheInvalidator
// consumes for all three resolver dimensions (project / user / call).
// Production wiring binds (*service.Cached{Project,User,Call}Resolver).Invalidate;
// tests substitute fakes.
type entityInvalidateFn func(id string)

// projectInvalidateFn is preserved as an alias for the public
// CacheInvalidatorConfig field type — the field name is part of the
// stable Plan 11.3 surface; renaming would force every existing
// caller to change. New fields use the canonical entityInvalidateFn.
type projectInvalidateFn = entityInvalidateFn

// CacheInvalidatorConfig is the construction surface for
// NewCacheInvalidator. Subscriber and ProjectInvalidate are
// required; the others have nil-safe defaults documented per-field.
type CacheInvalidatorConfig struct {
	// Subscriber is the eventbus we Subscribe to. Required —
	// nil panics at construction so a wiring bug surfaces at boot.
	Subscriber eventbus.Subscriber

	// ProjectInvalidate is the function called with every project_id
	// from a parsed crmapi.ProjectStatusChangedEvent. Required —
	// preserved from Plan 11.3 Task 3; nil panics at construction.
	ProjectInvalidate projectInvalidateFn

	// UserInvalidate is the function called with every user_id from
	// a parsed authapi.UserDeletedEvent. Optional — nil skips the
	// auth.user.deleted subscription with an INFO log (degraded boot
	// without auth wiring). Plan 11.4 Task 6.
	UserInvalidate entityInvalidateFn

	// CallInvalidate is the function called with every call_id from
	// a parsed rapi.RecordingCallDeletedEvent. Optional — nil skips
	// the recording.call.deleted subscription with an INFO log
	// (degraded boot without recording wiring). Plan 11.4 Task 6.
	CallInvalidate entityInvalidateFn

	// Metrics receives counter ticks. Nil-tolerated — observe is
	// a no-op on a nil *CacheInvalidatorMetrics receiver.
	Metrics *CacheInvalidatorMetrics

	// Logger is named for the cache-invalidator subsystem.
	// Nil-tolerated → zap.NewNop().
	Logger *zap.Logger

	// QueueGroup is the JetStream queue group joined for the
	// subscription. Default defaultCacheInvalidatorQueueGroup.
	// Tests pin a different name to scope subscriptions.
	QueueGroup string
}

// SubjectProjectStatus is the wildcard subject the cache invalidator
// subscribes to for project-status events. Built from
// crmapi.SubjectProjectStatus's "tenant.<t>.crm.project.status_changed"
// template by replacing the tenant placeholder with NATS's '*'
// single-token wildcard.
//
// Exported so module.go's log and tests can reference the same
// source of truth instead of duplicating the literal.
const SubjectProjectStatus = "tenant.*.crm.project.status_changed"

// SubjectUserDeleted is the wildcard subject for the auth.user.deleted
// subscription. Built from authapi.SubjectUserDeleted's
// "tenant.<t>.auth.user.deleted" template by replacing the tenant
// placeholder with NATS's '*' single-token wildcard.
const SubjectUserDeleted = "tenant.*.auth.user.deleted"

// SubjectRecordingCallDeleted is the wildcard subject for the
// recording.call.deleted subscription. Built from
// rapi.SubjectRecordingCallDeleted's
// "tenant.<t>.recording.call.deleted" template.
const SubjectRecordingCallDeleted = "tenant.*.recording.call.deleted"

// defaultCacheInvalidatorQueueGroup is the JetStream queue group
// used for the cache-invalidator subscription when none is
// specified. All replicas of cmd/api join the same group so the
// bus delivers each event to exactly ONE replica's handler — the
// cache-invalidation work is replica-local (every replica owns its
// own *CachedProjectResolver), so per-replica delivery would
// over-fan the work without changing correctness.
//
// Naming convention "realtime-<subsystem>" mirrors the existing
// realtime-replica-<id> queue groups in nats_subscriber.go.
const defaultCacheInvalidatorQueueGroup = "realtime-cache-invalidator"

// CacheInvalidator is the events-package handle for NATS-driven
// cache invalidation. Owns up to three Subscribes registered at Start.
// Subscriptions are torn down by the bus's Close (which drains
// every registered consumer goroutine). The ctx passed to Start
// only bounds the registration call itself; cmd/api passes
// context.Background() so a Register-scoped cancellation doesn't
// prematurely tear down the long-lived subscription.
type CacheInvalidator struct {
	cfg CacheInvalidatorConfig

	stopOnce sync.Once
}

// NewCacheInvalidator constructs a *CacheInvalidator. nil
// Subscriber or nil ProjectInvalidate panics — wiring bugs surface
// at boot. UserInvalidate and CallInvalidate are OPTIONAL: nil
// causes the matching subscription to be skipped at Start with an
// INFO log (degraded boot). Logger nil-safe; QueueGroup empty →
// defaultCacheInvalidatorQueueGroup.
func NewCacheInvalidator(cfg CacheInvalidatorConfig) *CacheInvalidator {
	if cfg.Subscriber == nil {
		panic("realtime/events: NewCacheInvalidator: Subscriber must be non-nil")
	}
	if cfg.ProjectInvalidate == nil {
		panic("realtime/events: NewCacheInvalidator: ProjectInvalidate must be non-nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.QueueGroup == "" {
		cfg.QueueGroup = defaultCacheInvalidatorQueueGroup
	}
	return &CacheInvalidator{cfg: cfg}
}

// Start registers every wired subscription. The project subscription
// is always registered; user/call subscriptions are skipped (with an
// INFO log) when their callback is nil so degraded boot remains
// operable. Returns the bus error wrapped with the package prefix +
// subject if any Subscribe fails — the composition root (module.go)
// logs WARN and falls back to TTL-only invalidation.
//
// Subscriptions are torn down by the bus's Close (which drains
// every registered consumer goroutine). The ctx passed to Start
// only bounds the registration call itself; cmd/api passes
// context.Background() so a Register-scoped cancellation doesn't
// prematurely tear down the long-lived subscription.
//
// Implementation note: the bus is push-mode; the handlers are
// invoked in goroutines the bus owns. *CacheInvalidator does
// NOT spawn its own goroutine (carry-forward of the
// NATSSubscriber pattern; Plan 11.1 Task 2).
func (c *CacheInvalidator) Start(ctx context.Context) error {
	if err := c.cfg.Subscriber.Subscribe(ctx, SubjectProjectStatus, c.cfg.QueueGroup, c.handleProject); err != nil {
		return fmt.Errorf("realtime/events: cache invalidator subscribe %q: %w", SubjectProjectStatus, err)
	}
	if c.cfg.UserInvalidate != nil {
		if err := c.cfg.Subscriber.Subscribe(ctx, SubjectUserDeleted, c.cfg.QueueGroup, c.handleUser); err != nil {
			return fmt.Errorf("realtime/events: cache invalidator subscribe %q: %w", SubjectUserDeleted, err)
		}
	} else {
		c.cfg.Logger.Info("realtime/events: cache invalidator: user subscription skipped (UserInvalidate nil)",
			zap.String("subject", SubjectUserDeleted),
		)
	}
	if c.cfg.CallInvalidate != nil {
		if err := c.cfg.Subscriber.Subscribe(ctx, SubjectRecordingCallDeleted, c.cfg.QueueGroup, c.handleCall); err != nil {
			return fmt.Errorf("realtime/events: cache invalidator subscribe %q: %w", SubjectRecordingCallDeleted, err)
		}
	} else {
		c.cfg.Logger.Info("realtime/events: cache invalidator: call subscription skipped (CallInvalidate nil)",
			zap.String("subject", SubjectRecordingCallDeleted),
		)
	}
	return nil
}

// Stop is the lifecycle teardown alias. Subscriptions are torn
// down by the bus's Close (which drains every registered consumer
// goroutine). The ctx passed to Start only bounds the registration
// call itself; cmd/api passes context.Background() so a
// Register-scoped cancellation doesn't prematurely tear down the
// long-lived subscription. Idempotent — second Stop is a no-op.
// Reserved for symmetry with NATSSubscriber + future extensions
// that may add a worker goroutine.
func (c *CacheInvalidator) Stop() {
	c.stopOnce.Do(func() {
		// Nothing to wait on — the bus owns the consumer goroutines.
	})
}

// handleProject is the per-message hook invoked by the bus for
// tenant.*.crm.project.status_changed. Always returns nil:
//
//   - success → cache.Delete + metric tick on result="ok";
//   - parse error → metric tick on result="parse_error", debug
//     log carrying ONLY the payload byte count (PII discipline);
//     a redelivery wouldn't change the parse outcome so ack.
//   - empty project_id → metric tick on result="empty_id";
//     defensive against a future schema bump that omits the
//     field. Cache lookups on the zero string would be no-ops
//     anyway, but a metric tick keeps the regression observable.
//
// Returning nil on every path is deliberate: a non-nil return
// triggers NATS redelivery, which against a permanently malformed
// payload is an infinite loop. Skip + observe + ack.
func (c *CacheInvalidator) handleProject(_ string, payload []byte) error {
	var ev crmapi.ProjectStatusChangedEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		c.cfg.Metrics.observe(SubjectProjectStatus, "parse_error")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop malformed project payload",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}
	if ev.ProjectID == uuid.Nil {
		c.cfg.Metrics.observe(SubjectProjectStatus, "empty_id")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop empty project_id",
			zap.Int("payload_bytes", len(payload)),
		)
		return nil
	}
	c.cfg.ProjectInvalidate(ev.ProjectID.String())
	c.cfg.Metrics.observe(SubjectProjectStatus, "ok")
	return nil
}

// handleUser is the per-message hook for tenant.*.auth.user.deleted.
// Same skip-observe-ack discipline as handleProject — see that doc
// comment for the full contract on result labels and redelivery.
func (c *CacheInvalidator) handleUser(_ string, payload []byte) error {
	var ev authapi.UserDeletedEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		c.cfg.Metrics.observe(SubjectUserDeleted, "parse_error")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop malformed user payload",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}
	if ev.UserID == uuid.Nil {
		c.cfg.Metrics.observe(SubjectUserDeleted, "empty_id")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop empty user_id",
			zap.Int("payload_bytes", len(payload)),
		)
		return nil
	}
	c.cfg.UserInvalidate(ev.UserID.String())
	c.cfg.Metrics.observe(SubjectUserDeleted, "ok")
	return nil
}

// handleCall is the per-message hook for tenant.*.recording.call.deleted.
// The cache key is the call_id, NOT the recording_id (the realtime
// CallResolver maps call_id → tenant_id; recordings are auxiliary).
// Same skip-observe-ack discipline as handleProject.
func (c *CacheInvalidator) handleCall(_ string, payload []byte) error {
	var ev rapi.RecordingCallDeletedEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		c.cfg.Metrics.observe(SubjectRecordingCallDeleted, "parse_error")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop malformed call payload",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}
	if ev.CallID == uuid.Nil {
		c.cfg.Metrics.observe(SubjectRecordingCallDeleted, "empty_id")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop empty call_id",
			zap.Int("payload_bytes", len(payload)),
		)
		return nil
	}
	c.cfg.CallInvalidate(ev.CallID.String())
	c.cfg.Metrics.observe(SubjectRecordingCallDeleted, "ok")
	return nil
}

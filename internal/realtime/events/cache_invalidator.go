// cache_invalidator.go subscribes to crm-side project lifecycle
// events and invalidates the realtime resolver cache so a project
// archive doesn't leave 60s of stale-cached cross-tenant approvals
// (the cache's lazy-expiry default in service/resolver_cache.go).
//
// Subject pattern: tenant.*.crm.project.status_changed
// Payload: crmapi.ProjectStatusChangedEvent{ProjectID, TenantID, ...}
//
// Future plans extend this when auth.user.deleted (Plan 11.4+
// candidate) and recording.call.deleted (Plan 12) ship — both
// follow the same shape.
//
// Carry-forward of the events package patterns from Plan 11
// Task 4b (NATSSubscriber) and Plan 11.1 Task 2 (TrunksReplicator):
// one Subscribe, one handler, metric tick on every dispatch
// outcome, no goroutines beyond the bus's push consumer.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"go.uber.org/zap"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// projectInvalidateFn is the narrow signature *CacheInvalidator
// consumes. (*service.CachedProjectResolver).Invalidate satisfies
// it via a method value; tests substitute a fake closure.
type projectInvalidateFn func(projectID string)

// CacheInvalidatorConfig is the construction surface for
// NewCacheInvalidator. Subscriber and ProjectInvalidate are
// required; the others have nil-safe defaults documented per-field.
type CacheInvalidatorConfig struct {
	// Subscriber is the eventbus we Subscribe to. Required —
	// nil panics at construction so a wiring bug surfaces at boot.
	Subscriber eventbus.Subscriber

	// ProjectInvalidate is the function called with every
	// project_id from a parsed ProjectStatusChangedEvent. The
	// production wiring binds this to
	// (*service.CachedProjectResolver).Invalidate — nil panics
	// at construction.
	ProjectInvalidate projectInvalidateFn

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
// subscribes to. Built from crmapi.SubjectProjectStatus's
// "tenant.<t>.crm.project.status_changed" template by replacing
// the tenant placeholder with NATS's '*' single-token wildcard.
//
// Exported so module.go's log and tests can reference the same
// source of truth instead of duplicating the literal.
const SubjectProjectStatus = "tenant.*.crm.project.status_changed"

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
// cache invalidation. Owns one Subscribe registered at Start.
// The subscription is torn down by the bus's Close (which drains
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
// at boot. Logger nil-safe; QueueGroup empty →
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

// Start registers the project-status subscription. Returns the bus
// error wrapped with the package prefix + subject if Subscribe
// fails — the composition root (module.go) logs WARN and falls
// back to TTL-only invalidation.
//
// The subscription is torn down by the bus's Close (which drains
// every registered consumer goroutine). The ctx passed to Start
// only bounds the registration call itself; cmd/api passes
// context.Background() so a Register-scoped cancellation doesn't
// prematurely tear down the long-lived subscription.
//
// Implementation note: the bus is push-mode; the handler is
// invoked in a goroutine the bus owns. *CacheInvalidator does
// NOT spawn its own goroutine (carry-forward of the
// NATSSubscriber pattern; Plan 11.1 Task 2).
func (c *CacheInvalidator) Start(ctx context.Context) error {
	if err := c.cfg.Subscriber.Subscribe(ctx, SubjectProjectStatus, c.cfg.QueueGroup, c.handle); err != nil {
		return fmt.Errorf("realtime/events: cache invalidator subscribe %q: %w", SubjectProjectStatus, err)
	}
	return nil
}

// Stop is the lifecycle teardown alias. The subscription is torn
// down by the bus's Close (which drains every registered consumer
// goroutine). The ctx passed to Start only bounds the registration
// call itself; cmd/api passes context.Background() so a
// Register-scoped cancellation doesn't prematurely tear down the
// long-lived subscription. Idempotent — second Stop is a no-op.
// Reserved for symmetry with NATSSubscriber + future extensions
// that may add a worker goroutine.
func (c *CacheInvalidator) Stop() {
	c.stopOnce.Do(func() {
		// Nothing to wait on — the bus owns the consumer goroutine.
	})
}

// handle is the per-message hook invoked by the bus. Always
// returns nil:
//
//   - success → cache.Delete + metric tick on result="ok";
//   - parse error → metric tick on result="parse_error", debug
//     log carrying ONLY the payload byte count (PII discipline);
//     a redelivery wouldn't change the parse outcome so ack.
//   - empty project_id → metric tick on result="empty_project_id";
//     defensive against a future schema bump that omits the
//     field. Cache lookups on the zero string would be no-ops
//     anyway, but a metric tick keeps the regression observable.
//
// Returning nil on every path is deliberate: a non-nil return
// triggers NATS redelivery, which against a permanently malformed
// payload is an infinite loop. Skip + observe + ack.
func (c *CacheInvalidator) handle(_ string, payload []byte) error {
	var ev crmapi.ProjectStatusChangedEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		c.cfg.Metrics.observe("parse_error")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop malformed payload",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}
	// Defensive: skip empty IDs. The crm publisher always sets
	// these but a future schema bump could omit; surface as a
	// metric tick rather than silent.
	if ev.ProjectID == uuid.Nil {
		c.cfg.Metrics.observe("empty_project_id")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop empty project_id",
			zap.Int("payload_bytes", len(payload)),
		)
		return nil
	}
	c.cfg.ProjectInvalidate(ev.ProjectID.String())
	c.cfg.Metrics.observe("ok")
	return nil
}

package events

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// TenantLister is the subset of tenancy.TenantService that
// *TrunksReplicator needs. The port is deliberately narrow so:
//
//   - production wiring can adapt tenancy.TenantService without test
//     code seeing the Tenant DTO,
//   - the lister owns its own caching policy (the replicator holds no
//     cached state — every Dispatch call re-asks the lister).
//
// Implementations cache aggressively in production: a 60s TTL is
// acceptable because a freshly-onboarded tenant misses at most one
// trunks.health event before its first cache miss.
type TenantLister interface {
	// ListActiveTenantIDs returns every active tenant's stringified
	// UUID. Empty result is acceptable boot-state (no tenants
	// onboarded yet).
	ListActiveTenantIDs(ctx context.Context) ([]string, error)
}

// TrunksReplicator owns the cross-tenant fan-out of the global
// `trunks.health` subject.
//
// The realtime Hub.Broadcast contract requires a non-empty TenantID
// (defence against cross-tenant leak). `trunks.health` is a global
// signal (FreeSWITCH cluster trunk states) without any tenant scope,
// so the replicator emits one Hub.Broadcast per active tenant by
// looking the catalog up through a TenantLister port.
//
// Lister failures are logged + counted via Metrics, but NEVER
// propagated to the bus — returning a non-nil error from the bus
// handler would trigger NATS redelivery, which against a permanently-
// broken catalog is an infinite loop. Skip + observe + ack instead.
//
// PII discipline: only the byte-count of the inbound payload may be
// logged. The payload itself is opaque trunk-state JSON; treating it
// as opaque keeps the log surface small even if a future schema
// change introduces tenant-scoped fields.
//
// The replicator owns no goroutines: Dispatch is invoked synchronously
// by the bus's push-consumer goroutine.
type TrunksReplicator struct {
	hub     HubBroadcaster
	lister  TenantLister
	logger  *zap.Logger
	metrics *Metrics
}

// NewTrunksReplicator constructs a *TrunksReplicator.
//
// hub and lister MUST be non-nil — passing nil for either PANICS at
// construction time. These are wiring bugs that we want to surface at
// boot rather than at first message dispatch (mirrors the *NATSSubscriber
// constructor convention from Plan 11 Task 4).
//
// logger nil-safe (defaults to zap.NewNop). metrics nil-safe (every
// observe* helper short-circuits on nil).
func NewTrunksReplicator(hub HubBroadcaster, lister TenantLister, logger *zap.Logger, metrics *Metrics) *TrunksReplicator {
	if hub == nil {
		panic("realtime/events: NewTrunksReplicator: hub must be non-nil")
	}
	if lister == nil {
		panic("realtime/events: NewTrunksReplicator: lister must be non-nil")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &TrunksReplicator{
		hub:     hub,
		lister:  lister,
		logger:  logger,
		metrics: metrics,
	}
}

// Dispatch is the inbound-message handler the bus subscriber wires to
// the `trunks.health` subject. The contract is:
//
//   - ALWAYS returns nil. Lister failure or no-active-tenants is
//     observed via metrics + logs, never propagated to the bus.
//   - For each active tenant, emits one Hub.Broadcast with TopicTrunksHealth
//     and a TenantID-only filter.
//   - Records one MessagesTotal increment + one FanoutSize observation
//     per Broadcast call so the existing dashboards work unchanged.
//   - The replicator does NOT cache the tenant list; the lister
//     implementation owns that policy.
//
// PII discipline: the payload is forwarded as a json.RawMessage and
// never inspected. Logs only carry the byte-count.
func (r *TrunksReplicator) Dispatch(ctx context.Context, payload []byte) error {
	tenants, err := r.lister.ListActiveTenantIDs(ctx)
	if err != nil {
		r.logger.Warn("realtime/events: trunks.health: tenant lister failed",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		r.metrics.observeDispatchFailure(string(rtapi.TopicTrunksHealth), reasonTenantListerFailed)
		return nil
	}
	if len(tenants) == 0 {
		// Boot ordering: until the first tenant is created, trunks.health
		// fans nothing out. Acceptable — debug-level only because this
		// is expected during the seconds between cmd/api boot and the
		// first POST /admin/tenants in fresh environments.
		r.logger.Debug("realtime/events: trunks.health: no active tenants — skipping fan-out",
			zap.Int("payload_bytes", len(payload)),
		)
		return nil
	}

	frame := json.RawMessage(payload)
	topicLab := string(rtapi.TopicTrunksHealth)
	for _, tenantID := range tenants {
		count := r.hub.Broadcast(ctx, rtapi.TopicTrunksHealth, frame, rtapi.BroadcastFilter{TenantID: tenantID})
		r.metrics.observeMessage(topicLab)
		r.metrics.observeFanout(count)
	}
	return nil
}

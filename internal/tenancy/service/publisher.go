package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// eventbusPublisher is the production api.SettingsPublisher backed by the
// project-wide eventbus.Publisher (NATS in production, noop in dev).
//
// Each method serialises a small JSON payload onto the canonical NATS
// subject defined in internal/tenancy/api/events.go and forwards it to the
// publisher. Failures are returned to the caller — TenantService logs them
// and continues so a transient broker outage never fails a Postgres-
// committed write.
type eventbusPublisher struct {
	logger *zap.Logger
	pub    eventbus.Publisher
}

// Compile-time assertion: eventbusPublisher satisfies api.SettingsPublisher.
var _ api.SettingsPublisher = (*eventbusPublisher)(nil)

// newPublisher constructs the canonical SettingsPublisher. The eventbus
// argument may be nil in dev builds that do not boot NATS — in that case
// every method becomes a no-op that logs at debug level so the absence is
// observable in dev.
func newPublisher(pub eventbus.Publisher, logger *zap.Logger) api.SettingsPublisher {
	return &eventbusPublisher{logger: logger, pub: pub}
}

func (p *eventbusPublisher) PublishCreated(ctx context.Context, t api.Tenant) error {
	return p.publish(ctx, api.SubjectTenantCreatedFor(t.ID), t)
}

func (p *eventbusPublisher) PublishSuspended(ctx context.Context, tenantID uuid.UUID) error {
	return p.publish(ctx, api.SubjectTenantSuspendedFor(tenantID),
		api.TenantSuspendedEvent{TenantID: tenantID})
}

func (p *eventbusPublisher) PublishArchived(ctx context.Context, tenantID uuid.UUID) error {
	return p.publish(ctx, api.SubjectTenantArchivedFor(tenantID),
		api.TenantArchivedEvent{TenantID: tenantID})
}

func (p *eventbusPublisher) PublishSettingUpdated(ctx context.Context, tenantID uuid.UUID, key string) error {
	return p.publish(ctx, api.SubjectSettingsUpdatedFor(tenantID),
		api.SettingsUpdatedEvent{TenantID: tenantID, Key: key})
}

func (p *eventbusPublisher) PublishSettingDeleted(ctx context.Context, tenantID uuid.UUID, key string) error {
	// Updated and deleted share a subject so peer caches invalidate
	// uniformly regardless of upsert vs delete semantics.
	return p.publish(ctx, api.SubjectSettingsUpdatedFor(tenantID),
		api.SettingsUpdatedEvent{TenantID: tenantID, Key: key})
}

func (p *eventbusPublisher) publish(ctx context.Context, subject string, payload any) error {
	if p.pub == nil {
		p.logger.Debug("eventbus publisher absent — skipping",
			zap.String("subject", subject),
		)
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("tenancy/service: marshal event %s: %w", subject, err)
	}
	if err := p.pub.Publish(ctx, subject, body); err != nil {
		return fmt.Errorf("tenancy/service: publish %s: %w", subject, err)
	}
	return nil
}

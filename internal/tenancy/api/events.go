package api

import (
	"fmt"

	"github.com/google/uuid"
)

// NATS subjects (best-effort core-NATS, no retention).
//
// Subject placeholder format from spec §10.2:
//
//	tenant.<t>.<event>
//
// Helpers below materialise concrete subjects.
const (
	// SubjectTenantCreated is the placeholder subject for tenant.<t>.created.
	SubjectTenantCreated = "tenant.<t>.created"
	// SubjectTenantSuspended is the placeholder subject for tenant.<t>.suspended.
	SubjectTenantSuspended = "tenant.<t>.suspended"
	// SubjectTenantResumed is the placeholder subject for tenant.<t>.resumed.
	SubjectTenantResumed = "tenant.<t>.resumed"
	// SubjectTenantArchived is the placeholder subject for tenant.<t>.archived.
	SubjectTenantArchived = "tenant.<t>.archived"
	// SubjectSettingsUpdated is the placeholder subject for tenant.<t>.settings.updated.
	// Peers subscribe to invalidate their local SettingsCache.
	SubjectSettingsUpdated = "tenant.<t>.settings.updated"
)

// SubjectTenantCreatedFor returns the concrete NATS subject for the
// tenant.created event for the given tenant.
func SubjectTenantCreatedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.created", tenantID)
}

// SubjectTenantSuspendedFor returns the concrete NATS subject for the
// tenant.suspended event for the given tenant.
func SubjectTenantSuspendedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.suspended", tenantID)
}

// SubjectTenantResumedFor returns the concrete NATS subject for the
// tenant.resumed event for the given tenant.
func SubjectTenantResumedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.resumed", tenantID)
}

// SubjectTenantArchivedFor returns the concrete NATS subject for the
// tenant.archived event for the given tenant.
func SubjectTenantArchivedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.archived", tenantID)
}

// SubjectSettingsUpdatedFor returns the concrete NATS subject used to publish
// (and consume) cache-invalidation messages for the given tenant.
func SubjectSettingsUpdatedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.settings.updated", tenantID)
}

// TenantSuspendedEvent is the payload for SubjectTenantSuspended.
type TenantSuspendedEvent struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Reason   string    `json:"reason"`
}

// TenantResumedEvent is the payload for SubjectTenantResumed.
type TenantResumedEvent struct {
	TenantID uuid.UUID `json:"tenant_id"`
}

// TenantArchivedEvent is the payload for SubjectTenantArchived.
type TenantArchivedEvent struct {
	TenantID uuid.UUID `json:"tenant_id"`
}

// SettingsUpdatedEvent is the payload for SubjectSettingsUpdated.
// Peers consume this to evict the matching key from their local cache.
type SettingsUpdatedEvent struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Key      string    `json:"key"`
}

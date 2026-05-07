package api

import (
	"fmt"

	"github.com/google/uuid"
)

// NATS subject for the durable JetStream stream AUDIT (90-day retention).
//
// Mirrored by analytics-ingestor for long-window queries; consumed by the
// cold-tier archiver after one year.
//
// Subject placeholder format from spec §10.2:
//
//	tenant.<t>.audit.event
const (
	// SubjectAuditEvent is the placeholder subject. The runtime materialises a
	// concrete subject via SubjectAuditEventFor.
	SubjectAuditEvent = "tenant.<t>.audit.event"
)

// SubjectAuditEventFor returns the concrete NATS subject used to publish an
// Event for the given tenant.
func SubjectAuditEventFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.audit.event", tenantID)
}

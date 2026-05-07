package api

import (
	"fmt"

	"github.com/google/uuid"
)

// NATS subject placeholders.
//
// realtime publishes (best-effort core-NATS):
//   - tenant.<t>.notify.user.<user_id> — in-app push routed to one user.
//
// Listen-in start/stop are mirrored to the audit module via the canonical
// audit subject; the constants below are the audit Action labels.
//
// realtime consumes (durable, explicit ack) the dialer + telephony streams
// and re-publishes them onto the local Hub topics. Those subject
// placeholders are owned by the upstream modules — see internal/dialer/api
// and internal/telephony/api.
const (
	// SubjectUserNotify is published to deliver an in-app notification to one user.
	// Owned here; the publisher fans out via the Hub.
	SubjectUserNotify = "tenant.<t>.notify.user.<user_id>"
)

// SubjectUserNotifyFor returns the concrete subject for a per-user notification.
func SubjectUserNotifyFor(tenantID, userID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.notify.user.%s", tenantID, userID)
}

// Audit action constants. realtime mirrors listen-in start/stop to the
// audit module via the canonical tenant.<t>.audit.event subject.
const (
	// AuditActionListenStarted is the audit Action set on ListenInService.Start.
	AuditActionListenStarted = "realtime.listen_started"
	// AuditActionListenStopped is the audit Action set on ListenInService.Stop.
	AuditActionListenStopped = "realtime.listen_stopped"
)

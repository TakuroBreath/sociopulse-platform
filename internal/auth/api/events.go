package api

import (
	"fmt"

	"github.com/google/uuid"
)

// Package api — auth module events.
//
// auth mirrors authentication-flow events (login, logout, TOTP enrolment,
// session revocation, refresh-token replay) to the audit module via the
// canonical `tenant.<t>.audit.event` subject (see internal/audit/api).
// The AuditAction* constants below are the labels written into
// audit.Event.Action.
//
// In addition to audit-mirroring, auth publishes user-lifecycle events on
// its OWN subjects (`tenant.<t>.auth.user.<verb>`) via the outbox pattern
// — these are events downstream services need to react to (cache
// invalidation, BI fan-out), not audit trail entries. Plan 11.4
// introduced the first such subject (`SubjectUserDeleted` below); future
// auth lifecycle events (user.created, user.role_changed, ...) will live
// on sibling subjects.
const (
	// AuditActionLogin is the audit Event.Action set on a successful login.
	AuditActionLogin = "auth.login"
	// AuditActionLogout is the audit Event.Action set on logout.
	AuditActionLogout = "auth.logout"
	// AuditActionTOTPEnrolled is the audit Event.Action set when TOTP enrolment is confirmed.
	AuditActionTOTPEnrolled = "auth.totp.enrolled"
	// AuditActionTOTPDisabled is the audit Event.Action set when a user disables TOTP.
	AuditActionTOTPDisabled = "auth.totp.disabled"
	// AuditActionTOTPVerified is the audit Event.Action set when a successful TOTP code is observed.
	AuditActionTOTPVerified = "auth.totp.verified"
	// AuditActionTOTPBackupUsed is the audit Event.Action set when a backup code is consumed.
	AuditActionTOTPBackupUsed = "auth.totp.backup_used"
	// AuditActionSessionRevoked is the audit Event.Action set on force-logout-all.
	AuditActionSessionRevoked = "auth.session_revoked"
	// AuditActionRefreshReplay is the audit Event.Action set when refresh-rotation reuse is detected.
	AuditActionRefreshReplay = "auth.refresh_replay"
)

// NATS subject placeholders for the durable JetStream stream AUTH
// (auth events stream — created by infra alongside the existing
// CRM/RECORDING streams). The "<t>" placeholder is for documentation
// only — code MUST use the SubjectUserDeletedFor helper to render the
// concrete subject for a tenant.
//
// Plan 11.4 introduces the FIRST auth NATS subject. Future auth
// lifecycle events (user.created, user.role_changed, ...) belong on
// sibling tenant.<t>.auth.user.<verb> subjects.
const (
	// SubjectUserDeleted is published after a successful UserService.Archive.
	// Subscribers (currently the realtime CacheInvalidator) treat this as
	// "the user can no longer authenticate; drop any cached entry referring
	// to them". A future hard-delete path would emit the same subject with
	// Reason="hard_deleted".
	SubjectUserDeleted = "tenant.<t>.auth.user.deleted"
)

// SubjectUserDeletedFor returns the concrete NATS subject for the
// auth.user.deleted event for the given tenant. Mirrors the
// crmapi.SubjectProjectStatusFor / recordingapi.SubjectRecordingCallDeletedFor
// pattern.
func SubjectUserDeletedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.auth.user.deleted", tenantID)
}

// UserDeletedEvent is the payload published on
// SubjectUserDeletedFor(tenantID) after UserService.Archive (and any
// future hard-delete path) commits. Subscribers MUST treat the user
// as no longer authenticatable — any cached (user_id → tenant_id)
// resolver entry should be dropped.
//
// Reason is "archived" for the v1 soft-delete path (the only path
// that exists today). A future hard-delete admin endpoint would emit
// "hard_deleted"; archive code paths MUST NOT use that value.
//
// Carries opaque UUIDs only — no PII (phone numbers, emails, names)
// crosses the bus.
type UserDeletedEvent struct {
	UserID    uuid.UUID `json:"user_id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	DeletedAt int64     `json:"deleted_at"` // unix seconds
	Reason    string    `json:"reason"`     // "archived" | "hard_deleted"
}

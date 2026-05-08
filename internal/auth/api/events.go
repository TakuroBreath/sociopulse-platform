package api

// Package api — auth module events.
//
// auth does not publish events on its own subjects. Instead, every successful
// login, logout, TOTP enrolment, session revocation, and refresh-token replay
// is mirrored to the audit module via the canonical
// `tenant.<t>.audit.event` subject (see internal/audit/api). The Action
// constants below are the action labels written into audit.Event.Action.
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

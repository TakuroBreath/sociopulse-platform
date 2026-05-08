package api

import "github.com/google/uuid"

// Action constants for the static RBAC matrix.
//
// Conventions:
//   - One constant per (resource, verb) pair, in dotted form
//     ("user.create", "recording.download").
//   - Values are low-cardinality strings safe to log or surface in metrics
//     and audit records.
//   - Resource-id self-checks (e.g. operator changing their OWN password
//     vs. another user's) are NOT enforced at the matrix layer — that is
//     the caller's responsibility because Resource.ID matters. The matrix
//     answers role × action only.
//
// Source: spec §12.7 RBAC table, refined by Plan 05 Task 7 to align with
// the resource verbs used by REST and WS endpoints in plans 06–13.
const (
	// User administration — admin only.
	ActionUserCreate        Action = "user.create"
	ActionUserList          Action = "user.list"
	ActionUserGet           Action = "user.get"
	ActionUserUpdate        Action = "user.update"
	ActionUserArchive       Action = "user.archive"
	ActionUserResetPassword Action = "user.reset_password"

	// Self-service — every authenticated user. Resource-id ownership
	// checks are still the caller's responsibility.
	ActionSelfChangePassword Action = "self.change_password"
	ActionSelfTOTPEnroll     Action = "self.totp_enroll"
	ActionSelfTOTPDisable    Action = "self.totp_disable"

	// Projects (FR-C in spec §11).
	ActionProjectCreate         Action = "project.create"
	ActionProjectList           Action = "project.list"
	ActionProjectAssignOperator Action = "project.assign_operator"

	// Calls. Operators run them; supervisors (and admins) also monitor.
	ActionCallStart  Action = "call.start"
	ActionCallEnd    Action = "call.end"
	ActionCallListen Action = "call.listen" // live monitor
	ActionCallList   Action = "call.list"

	// Recordings — supervisor + admin (operator has no list/download).
	ActionRecordingDownload Action = "recording.download"
	ActionRecordingList     Action = "recording.list"

	// Reports — supervisor + admin.
	ActionReportGenerate Action = "report.generate"
	ActionReportList     Action = "report.list"
)

// ResourceUser returns a Resource pointing at the user with the given ID.
// Helper used by callers that need to attach an explicit user ID for
// downstream ownership checks. The matrix layer ignores Resource.ID.
func ResourceUser(id uuid.UUID) Resource {
	return Resource{Kind: "user", ID: id}
}

// ResourceTenantWide returns a Resource for tenant-scoped actions that do
// not target a specific instance (Resource.ID is the zero UUID).
func ResourceTenantWide(kind string) Resource {
	return Resource{Kind: kind}
}

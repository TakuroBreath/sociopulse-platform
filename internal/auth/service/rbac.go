// Package service: rbac.go contains RBACMatrix, the static-table
// implementation of api.RBACChecker.
//
// The matrix is a compile-time constant: every (Role -> set of allowed
// Actions) entry is enumerated explicitly with no hard-coded inheritance.
// Reading the source is therefore a complete audit; nothing is implied.
//
// Resource-id self-checks (e.g. user changing their OWN password vs.
// another user's) are NOT enforced here — that is the caller's job. The
// matrix answers role × action only and ignores Resource entirely.
package service

import (
	"context"
	"fmt"

	authapi "github.com/sociopulse/platform/internal/auth/api"
)

// rbacMatrix maps each Role to its allow-set. Membership in the inner
// set means "this role permits this action". A user with multiple roles
// passes Check if ANY of their roles allows the action (union semantics).
//
// Auditability: every role's allow-set is enumerated explicitly. The
// admin set is NOT defined as "supervisor + extras"; it lists every
// allowed action so a single grep tells the whole story.
var rbacMatrix = map[authapi.Role]map[authapi.Action]struct{}{
	authapi.RoleOperator: setOf(
		// Self-service.
		authapi.ActionSelfChangePassword,
		authapi.ActionSelfTOTPEnroll,
		authapi.ActionSelfTOTPDisable,
		// Projects: list (caller scopes to own assignments).
		authapi.ActionProjectList,
		// Calls: operator runs calls.
		authapi.ActionCallStart,
		authapi.ActionCallEnd,
	),
	authapi.RoleSupervisor: setOf(
		// Self-service.
		authapi.ActionSelfChangePassword,
		authapi.ActionSelfTOTPEnroll,
		authapi.ActionSelfTOTPDisable,
		// Projects.
		authapi.ActionProjectList,
		authapi.ActionProjectAssignOperator,
		// Calls: monitor + run.
		authapi.ActionCallStart,
		authapi.ActionCallEnd,
		authapi.ActionCallListen,
		authapi.ActionCallList,
		// Recordings.
		authapi.ActionRecordingList,
		authapi.ActionRecordingDownload,
		// Reports.
		authapi.ActionReportGenerate,
		authapi.ActionReportList,
		// Billing — view only; tariff updates remain admin-only.
		authapi.ActionBillingView,
	),
	authapi.RoleAdmin: setOf(
		// User administration.
		authapi.ActionUserCreate,
		authapi.ActionUserList,
		authapi.ActionUserGet,
		authapi.ActionUserUpdate,
		authapi.ActionUserArchive,
		authapi.ActionUserResetPassword,
		// Self-service.
		authapi.ActionSelfChangePassword,
		authapi.ActionSelfTOTPEnroll,
		authapi.ActionSelfTOTPDisable,
		// Projects.
		authapi.ActionProjectCreate,
		authapi.ActionProjectList,
		authapi.ActionProjectAssignOperator,
		// Calls.
		authapi.ActionCallStart,
		authapi.ActionCallEnd,
		authapi.ActionCallListen,
		authapi.ActionCallList,
		// Recordings.
		authapi.ActionRecordingList,
		authapi.ActionRecordingDownload,
		// Reports.
		authapi.ActionReportGenerate,
		authapi.ActionReportList,
		// Billing — full access (view + tariff updates).
		authapi.ActionBillingView,
		authapi.ActionBillingTariffUpdate,
	),
}

// setOf builds a string-set from the given actions.
func setOf(actions ...authapi.Action) map[authapi.Action]struct{} {
	m := make(map[authapi.Action]struct{}, len(actions))
	for _, a := range actions {
		m[a] = struct{}{}
	}
	return m
}

// RBACMatrix is the static-table implementation of api.RBACChecker.
// Stateless and safe to share across goroutines; the underlying map is
// never written after package initialisation.
type RBACMatrix struct{}

// NewRBACMatrix returns a ready-to-use RBACMatrix.
func NewRBACMatrix() *RBACMatrix { return &RBACMatrix{} }

// Check returns nil if any of claims.Roles permits action; otherwise
// it returns an error wrapping api.ErrInsufficientRole. The ctx and
// resource arguments are accepted to satisfy the api.RBACChecker
// interface but are intentionally unused: the matrix layer answers
// role × action only.
func (r *RBACMatrix) Check(_ context.Context, claims authapi.Claims, action authapi.Action, _ authapi.Resource) error {
	for _, role := range claims.Roles {
		perms, ok := rbacMatrix[role]
		if !ok {
			// Unknown role — ignored, fall through to next.
			continue
		}
		if _, allowed := perms[action]; allowed {
			return nil
		}
	}
	return fmt.Errorf("rbac: %w: roles %v cannot %s", authapi.ErrInsufficientRole, claims.Roles, action)
}

// Compile-time assertion that *RBACMatrix satisfies api.RBACChecker.
var _ authapi.RBACChecker = (*RBACMatrix)(nil)

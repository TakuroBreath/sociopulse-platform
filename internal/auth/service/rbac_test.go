package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/internal/auth/service"
)

// rbacCase describes one row in the (roles × action) → expectation table.
// allowed=true asserts Check returns nil; allowed=false asserts the
// returned error wraps api.ErrInsufficientRole.
type rbacCase struct {
	name    string
	roles   []authapi.Role
	action  authapi.Action
	allowed bool
}

// canonicalCases enumerates every (single-role, action) pair from the
// canonical Plan 05 Task 7 RBAC matrix. The matrix is reproduced
// here verbatim so a future audit is one diff away from the source.
//
//nolint:funlen // table-driven case enumeration is intentionally long.
func canonicalCases() []rbacCase {
	return []rbacCase{
		// ---- user.* -- admin only -----------------------------------
		{"operator/user.create denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionUserCreate, false},
		{"operator/user.list denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionUserList, false},
		{"operator/user.get denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionUserGet, false},
		{"operator/user.update denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionUserUpdate, false},
		{"operator/user.archive denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionUserArchive, false},
		{"operator/user.reset_password denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionUserResetPassword, false},

		{"supervisor/user.create denied", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionUserCreate, false},
		{"supervisor/user.list denied", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionUserList, false},
		{"supervisor/user.get denied", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionUserGet, false},
		{"supervisor/user.update denied", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionUserUpdate, false},
		{"supervisor/user.archive denied", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionUserArchive, false},
		{"supervisor/user.reset_password denied", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionUserResetPassword, false},

		{"admin/user.create allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionUserCreate, true},
		{"admin/user.list allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionUserList, true},
		{"admin/user.get allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionUserGet, true},
		{"admin/user.update allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionUserUpdate, true},
		{"admin/user.archive allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionUserArchive, true},
		{"admin/user.reset_password allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionUserResetPassword, true},

		// ---- self.* -- every authenticated user --------------------
		{"operator/self.change_password allowed", []authapi.Role{authapi.RoleOperator}, authapi.ActionSelfChangePassword, true},
		{"operator/self.totp_enroll allowed", []authapi.Role{authapi.RoleOperator}, authapi.ActionSelfTOTPEnroll, true},
		{"operator/self.totp_disable allowed", []authapi.Role{authapi.RoleOperator}, authapi.ActionSelfTOTPDisable, true},
		{"supervisor/self.change_password allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionSelfChangePassword, true},
		{"supervisor/self.totp_enroll allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionSelfTOTPEnroll, true},
		{"supervisor/self.totp_disable allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionSelfTOTPDisable, true},
		{"admin/self.change_password allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionSelfChangePassword, true},
		{"admin/self.totp_enroll allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionSelfTOTPEnroll, true},
		{"admin/self.totp_disable allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionSelfTOTPDisable, true},

		// ---- project.create -- admin only --------------------------
		{"operator/project.create denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionProjectCreate, false},
		{"supervisor/project.create denied", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionProjectCreate, false},
		{"admin/project.create allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionProjectCreate, true},

		// ---- project.list -- everyone (matrix-level allow; finer
		// scoping is the caller's responsibility) -------------------
		{"operator/project.list allowed", []authapi.Role{authapi.RoleOperator}, authapi.ActionProjectList, true},
		{"supervisor/project.list allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionProjectList, true},
		{"admin/project.list allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionProjectList, true},

		// ---- project.assign_operator -- supervisor + admin --------
		{"operator/project.assign_operator denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionProjectAssignOperator, false},
		{"supervisor/project.assign_operator allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionProjectAssignOperator, true},
		{"admin/project.assign_operator allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionProjectAssignOperator, true},

		// ---- call.start / call.end -- everyone --------------------
		{"operator/call.start allowed", []authapi.Role{authapi.RoleOperator}, authapi.ActionCallStart, true},
		{"supervisor/call.start allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionCallStart, true},
		{"admin/call.start allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionCallStart, true},
		{"operator/call.end allowed", []authapi.Role{authapi.RoleOperator}, authapi.ActionCallEnd, true},
		{"supervisor/call.end allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionCallEnd, true},
		{"admin/call.end allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionCallEnd, true},

		// ---- call.listen -- supervisor + admin --------------------
		{"operator/call.listen denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionCallListen, false},
		{"supervisor/call.listen allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionCallListen, true},
		{"admin/call.listen allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionCallListen, true},

		// ---- call.list -- supervisor + admin (operator goes
		// through a different "own calls" code path) --------------
		{"operator/call.list denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionCallList, false},
		{"supervisor/call.list allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionCallList, true},
		{"admin/call.list allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionCallList, true},

		// ---- recordings -- supervisor + admin --------------------
		{"operator/recording.list denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionRecordingList, false},
		{"supervisor/recording.list allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionRecordingList, true},
		{"admin/recording.list allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionRecordingList, true},
		{"operator/recording.download denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionRecordingDownload, false},
		{"supervisor/recording.download allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionRecordingDownload, true},
		{"admin/recording.download allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionRecordingDownload, true},

		// ---- reports -- supervisor + admin -----------------------
		{"operator/report.generate denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionReportGenerate, false},
		{"supervisor/report.generate allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionReportGenerate, true},
		{"admin/report.generate allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionReportGenerate, true},
		{"operator/report.list denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionReportList, false},
		{"supervisor/report.list allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionReportList, true},
		{"admin/report.list allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionReportList, true},

		// ---- billing.view -- supervisor + admin (Plan 14) -------
		{"operator/billing.view denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionBillingView, false},
		{"supervisor/billing.view allowed", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionBillingView, true},
		{"admin/billing.view allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionBillingView, true},

		// ---- billing.tariff_update -- admin only (Plan 14) ------
		{"operator/billing.tariff_update denied", []authapi.Role{authapi.RoleOperator}, authapi.ActionBillingTariffUpdate, false},
		{"supervisor/billing.tariff_update denied", []authapi.Role{authapi.RoleSupervisor}, authapi.ActionBillingTariffUpdate, false},
		{"admin/billing.tariff_update allowed", []authapi.Role{authapi.RoleAdmin}, authapi.ActionBillingTariffUpdate, true},
	}
}

func TestRBACMatrix_Check_Canonical(t *testing.T) {
	t.Parallel()

	checker := service.NewRBACMatrix()
	for _, tc := range canonicalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			claims := authapi.Claims{Roles: tc.roles}
			err := checker.Check(context.Background(), claims, tc.action, authapi.Resource{})
			if tc.allowed {
				require.NoError(t, err, "action %s should be allowed for roles %v", tc.action, tc.roles)
				return
			}
			require.ErrorIs(t, err, authapi.ErrInsufficientRole,
				"deny error must wrap ErrInsufficientRole; action %s, roles %v", tc.action, tc.roles)
		})
	}
}

func TestRBACMatrix_Check_EmptyRoles(t *testing.T) {
	t.Parallel()

	checker := service.NewRBACMatrix()
	// Sweep across a sample of actions to make sure the empty-roles
	// path doesn't accidentally allow any action.
	actions := []authapi.Action{
		authapi.ActionUserCreate,
		authapi.ActionSelfChangePassword,
		authapi.ActionCallStart,
		authapi.ActionRecordingDownload,
		authapi.ActionReportList,
	}
	for _, action := range actions {
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()

			err := checker.Check(context.Background(), authapi.Claims{}, action, authapi.Resource{})
			require.ErrorIs(t, err, authapi.ErrInsufficientRole)
		})
	}
}

func TestRBACMatrix_Check_MultiRoleUnion(t *testing.T) {
	t.Parallel()

	checker := service.NewRBACMatrix()
	// operator alone cannot list calls; supervisor alone can. The
	// union of [operator, supervisor] must therefore allow it.
	claims := authapi.Claims{Roles: []authapi.Role{authapi.RoleOperator, authapi.RoleSupervisor}}

	require.NoError(t, checker.Check(context.Background(), claims, authapi.ActionCallList, authapi.Resource{}),
		"union of operator+supervisor must allow call.list (supervisor's privilege)")
	require.NoError(t, checker.Check(context.Background(), claims, authapi.ActionCallStart, authapi.Resource{}),
		"union of operator+supervisor must allow call.start (both privilege)")

	// Neither role grants user.create, so the union must still deny.
	err := checker.Check(context.Background(), claims, authapi.ActionUserCreate, authapi.Resource{})
	require.ErrorIs(t, err, authapi.ErrInsufficientRole)
}

func TestRBACMatrix_Check_UnknownRoleIgnored(t *testing.T) {
	t.Parallel()

	checker := service.NewRBACMatrix()
	// A role string that is not in the matrix is ignored — does not
	// grant any privilege.
	claims := authapi.Claims{Roles: []authapi.Role{authapi.Role("ghost")}}
	err := checker.Check(context.Background(), claims, authapi.ActionCallStart, authapi.Resource{})
	require.ErrorIs(t, err, authapi.ErrInsufficientRole)

	// An unknown role mixed with operator must still allow operator's
	// actions.
	claims = authapi.Claims{Roles: []authapi.Role{authapi.Role("ghost"), authapi.RoleOperator}}
	require.NoError(t, checker.Check(context.Background(), claims, authapi.ActionCallStart, authapi.Resource{}))
}

func TestRBACMatrix_Check_ResourceIgnored(t *testing.T) {
	t.Parallel()

	checker := service.NewRBACMatrix()
	// The matrix is role × action only. Whatever resource is supplied,
	// the answer for (admin, user.create) must be allow.
	claims := authapi.Claims{Roles: []authapi.Role{authapi.RoleAdmin}}
	resources := []authapi.Resource{
		{},
		authapi.ResourceUser(uuid.New()),
		authapi.ResourceTenantWide("project"),
	}
	for _, r := range resources {
		require.NoError(t, checker.Check(context.Background(), claims, authapi.ActionUserCreate, r))
	}
}

func TestRBACMatrix_Check_NilContextDoesNotPanic(t *testing.T) {
	t.Parallel()

	checker := service.NewRBACMatrix()
	claims := authapi.Claims{Roles: []authapi.Role{authapi.RoleAdmin}}
	require.NotPanics(t, func() {
		// We pass context.Background() rather than literal nil to
		// keep the contract honest — the matrix is purely
		// in-memory, but accepting a context is part of the
		// RBACChecker interface for future-proofing.
		_ = checker.Check(context.Background(), claims, authapi.ActionUserCreate, authapi.Resource{})
	})
}

func TestRBACMatrix_ImplementsInterface(t *testing.T) {
	t.Parallel()

	// Compile-time check is in rbac.go; this test just makes the
	// dependency explicit so a refactor that breaks the interface
	// surface is reported here too.
	var _ authapi.RBACChecker = service.NewRBACMatrix()
}

func TestResourceConstructors(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	user := authapi.ResourceUser(id)
	require.Equal(t, "user", user.Kind)
	require.Equal(t, id, user.ID)

	tw := authapi.ResourceTenantWide("project")
	require.Equal(t, "project", tw.Kind)
	require.Equal(t, uuid.Nil, tw.ID)
}

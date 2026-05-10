// rbac_test.go — exhaustive coverage of the per-topic RBAC matrix.
//
// Why exhaustive: TopicRBAC is the policy gate every Subscribe call
// flows through. A silent matrix-row drop (typo, missing role) would
// either let an operator subscribe to TopicTrunksHealth (security
// regression) or block an admin from TopicCallEvents (service
// regression). The matrix is small enough to enumerate every
// (topic × role × filter) tuple — we do.
package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// stubResolver returns a fixed TenantID for any input. Used in the
// cross-tenant tests below; the real CachedUserResolver wires this
// through to auth.UserStore in production.
type stubResolver struct {
	tenantByID map[string]string
}

func (s *stubResolver) Get(_ context.Context, id string) (rtapi.ResolvedTenant, error) {
	tid, ok := s.tenantByID[id]
	if !ok {
		return rtapi.ResolvedTenant{}, errors.New("not found")
	}
	return rtapi.ResolvedTenant{TenantID: tid}, nil
}

func TestRBAC_OperatorCannotSubscribeOperatorsState(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"operator"}},
		rtapi.TopicOperatorsState,
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)
}

func TestRBAC_AdminCanSubscribeOperatorsState(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"admin"}},
		rtapi.TopicOperatorsState,
		rtapi.SubscriptionFilter{},
	)
	require.NoError(t, err)
}

func TestRBAC_SupervisorCanSubscribeOperatorsState(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"supervisor"}},
		rtapi.TopicOperatorsState,
		rtapi.SubscriptionFilter{},
	)
	require.NoError(t, err)
}

func TestRBAC_OperatorCannotSubscribeDialerQueue(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"operator"}},
		rtapi.TopicDialerQueue,
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)
}

func TestRBAC_AdminCanSubscribeDialerQueue(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"admin"}},
		rtapi.TopicDialerQueue,
		rtapi.SubscriptionFilter{},
	)
	require.NoError(t, err)
}

func TestRBAC_SupervisorCanSubscribeDialerQueue(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"supervisor"}},
		rtapi.TopicDialerQueue,
		rtapi.SubscriptionFilter{},
	)
	require.NoError(t, err)
}

func TestRBAC_OperatorCannotSubscribeTrunksHealth(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"operator"}},
		rtapi.TopicTrunksHealth,
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)
}

func TestRBAC_SupervisorCannotSubscribeTrunksHealth(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"supervisor"}},
		rtapi.TopicTrunksHealth,
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)
}

func TestRBAC_AdminCanSubscribeTrunksHealth(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"admin"}},
		rtapi.TopicTrunksHealth,
		rtapi.SubscriptionFilter{},
	)
	require.NoError(t, err)
}

func TestRBAC_CallEvents_RequiresCallID(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"admin"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrFilterRequired)
}

func TestRBAC_CallEvents_OperatorWithCallIDAllowed(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"operator"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "call-1"},
	)
	require.NoError(t, err)
}

func TestRBAC_CallEvents_AdminWithCallIDAllowed(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"admin"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "call-1"},
	)
	require.NoError(t, err)
}

func TestRBAC_CallEvents_SupervisorWithCallIDAllowed(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"supervisor"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "call-1"},
	)
	require.NoError(t, err)
}

func TestRBAC_NotificationsUser_SelfOnly_AllowsSelf(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	// OperatorID == UserID is the self case → allowed.
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"operator"}},
		rtapi.TopicNotifications,
		rtapi.SubscriptionFilter{OperatorID: "u1"},
	)
	require.NoError(t, err)
}

func TestRBAC_NotificationsUser_SelfOnly_AllowsEmptyOperatorID(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	// Empty OperatorID == "self by default" → allowed.
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"operator"}},
		rtapi.TopicNotifications,
		rtapi.SubscriptionFilter{},
	)
	require.NoError(t, err)
}

func TestRBAC_NotificationsUser_SelfOnly_RejectsForeign(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	// Even an admin cannot subscribe to another user's notifications.
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"admin"}},
		rtapi.TopicNotifications,
		rtapi.SubscriptionFilter{OperatorID: "u2"},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)
}

func TestRBAC_ForceCommands_SelfOnly_AllowsSelf(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"operator"}},
		rtapi.TopicForceCommands,
		rtapi.SubscriptionFilter{OperatorID: "u1"},
	)
	require.NoError(t, err)
}

func TestRBAC_ForceCommands_SelfOnly_AllowsEmptyOperatorID(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"operator"}},
		rtapi.TopicForceCommands,
		rtapi.SubscriptionFilter{},
	)
	require.NoError(t, err)
}

func TestRBAC_ForceCommands_SelfOnly_RejectsForeign(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"supervisor"}},
		rtapi.TopicForceCommands,
		rtapi.SubscriptionFilter{OperatorID: "u2"},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)
}

func TestRBAC_UnknownTopic(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"admin"}},
		rtapi.Topic("not.a.topic"),
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrUnknownTopic)
}

func TestRBAC_SelfOnly_EmptyUserIDDenied_Notifications(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	// claims.UserID == "" with selfOnly topic must be denied — even
	// with a non-empty role and empty filter.OperatorID. Defence-in-
	// depth against an upstream auth bug that planted empty UserID.
	err := r.Allow(
		t.Context(),
		rtapi.Claims{Roles: []string{"operator"}},
		rtapi.TopicNotifications,
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)
}

func TestRBAC_SelfOnly_EmptyUserIDDenied_ForceCommands(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
		t.Context(),
		rtapi.Claims{Roles: []string{"supervisor"}},
		rtapi.TopicForceCommands,
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)
}

func TestRBAC_NoRoles_DeniedEverywhere(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()

	// User with no roles must be denied for every topic that has a
	// non-empty allowedRoles set.
	for _, topic := range rtapi.AllTopics {
		t.Run(string(topic), func(t *testing.T) {
			t.Parallel()
			err := r.Allow(
				t.Context(),
				rtapi.Claims{UserID: "u1"},
				topic,
				rtapi.SubscriptionFilter{CallID: "c1", OperatorID: "u1"},
			)
			require.ErrorIs(t, err, service.ErrTopicForbidden)
		})
	}
}

func TestRBAC_MultiRole_UnionApplies(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	// User has both supervisor + operator. TopicTrunksHealth requires
	// admin only → still denied.
	err := r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"supervisor", "operator"}},
		rtapi.TopicTrunksHealth,
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)

	// But TopicOperatorsState (admin + supervisor) → allowed via
	// supervisor role.
	err = r.Allow(
		t.Context(),
		rtapi.Claims{UserID: "u1", Roles: []string{"supervisor", "operator"}},
		rtapi.TopicOperatorsState,
		rtapi.SubscriptionFilter{},
	)
	require.NoError(t, err)
}

// TestTopicRBAC_RejectsCrossTenantOperatorFilter verifies that a
// supervisor in tenant A subscribing to operators.state with an
// OperatorID belonging to tenant B is rejected with
// ErrCrossTenantSubscribe.
func TestTopicRBAC_RejectsCrossTenantOperatorFilter(t *testing.T) {
	t.Parallel()

	users := &stubResolver{tenantByID: map[string]string{
		"victim-op": "tenant-B", // foreign tenant
	}}
	rbac := service.NewTopicRBACWithResolvers(users, nil)

	claims := rtapi.Claims{
		UserID:   "attacker",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}
	filter := rtapi.SubscriptionFilter{OperatorID: "victim-op"}

	err := rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState, filter)
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrCrossTenantSubscribe,
		"cross-tenant OperatorID must reject with ErrCrossTenantSubscribe")
}

// TestTopicRBAC_RejectsCrossTenantProjectFilter mirrors the operator
// case for project_id filters.
func TestTopicRBAC_RejectsCrossTenantProjectFilter(t *testing.T) {
	t.Parallel()

	projects := &stubResolver{tenantByID: map[string]string{
		"foreign-project": "tenant-B",
	}}
	rbac := service.NewTopicRBACWithResolvers(nil, projects)

	claims := rtapi.Claims{
		UserID:   "u1",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}
	filter := rtapi.SubscriptionFilter{ProjectID: "foreign-project"}

	err := rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState, filter)
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrCrossTenantSubscribe)
}

// TestTopicRBAC_AllowsSameTenantFilters is the happy path: filter
// UUIDs whose TenantID matches the subscriber's claims pass.
func TestTopicRBAC_AllowsSameTenantFilters(t *testing.T) {
	t.Parallel()

	users := &stubResolver{tenantByID: map[string]string{
		"my-op": "tenant-A",
	}}
	projects := &stubResolver{tenantByID: map[string]string{
		"my-project": "tenant-A",
	}}
	rbac := service.NewTopicRBACWithResolvers(users, projects)

	claims := rtapi.Claims{
		UserID:   "supervisor",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}

	require.NoError(t, rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState,
		rtapi.SubscriptionFilter{OperatorID: "my-op", ProjectID: "my-project"}))
}

// TestTopicRBAC_AllowsZeroResolverFallback verifies that the legacy
// NewTopicRBAC() (no resolvers) preserves Plan 11 behaviour: the
// cross-tenant check is skipped entirely when resolvers are absent.
func TestTopicRBAC_AllowsZeroResolverFallback(t *testing.T) {
	t.Parallel()

	rbac := service.NewTopicRBAC() // legacy constructor

	claims := rtapi.Claims{
		UserID:   "u1",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}
	// Foreign-looking UUIDs — Allow must NOT reject because the
	// resolver wasn't wired.
	filter := rtapi.SubscriptionFilter{
		OperatorID: "this-could-be-any-tenant",
		ProjectID:  "another-arbitrary-id",
	}

	require.NoError(t, rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState, filter))
}

// TestTopicRBAC_FoldsResolverErrorIntoCrossTenant is the security
// guarantee: a resolver error (not-found, DB error) MUST surface as
// ErrCrossTenantSubscribe so the wire response is indistinguishable
// from a real cross-tenant attempt — the client cannot probe entity
// existence.
func TestTopicRBAC_FoldsResolverErrorIntoCrossTenant(t *testing.T) {
	t.Parallel()

	users := &stubResolver{tenantByID: map[string]string{}} // empty → all lookups fail
	rbac := service.NewTopicRBACWithResolvers(users, nil)

	claims := rtapi.Claims{
		UserID:   "u1",
		TenantID: "tenant-A",
		Roles:    []string{"supervisor"},
	}
	err := rbac.Allow(t.Context(), claims, rtapi.TopicOperatorsState,
		rtapi.SubscriptionFilter{OperatorID: "nonexistent"})
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrCrossTenantSubscribe,
		"resolver error must fold into ErrCrossTenantSubscribe (don't leak entity existence)")
}

// TestTopicRBAC_AllowRejectsCrossTenantCallID verifies the new Plan 11.4
// CallResolver dimension. A call_id whose tenant differs from the
// subscriber's claims must be rejected with ErrCrossTenantSubscribe.
func TestTopicRBAC_AllowRejectsCrossTenantCallID(t *testing.T) {
	t.Parallel()

	calls := &stubResolver{tenantByID: map[string]string{
		"call-other-tenant": "tenant-B",
	}}
	rbac := service.NewTopicRBACWithCallResolver(nil, nil, calls)

	err := rbac.Allow(t.Context(),
		rtapi.Claims{TenantID: "tenant-A", UserID: "u-1", Roles: []string{"operator"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "call-other-tenant"},
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrCrossTenantSubscribe,
		"cross-tenant call subscribe must be folded into ErrCrossTenantSubscribe")
}

// TestTopicRBAC_AllowAcceptsSameTenantCallID verifies the happy path —
// a CallID whose tenant matches the subscriber's claims passes.
func TestTopicRBAC_AllowAcceptsSameTenantCallID(t *testing.T) {
	t.Parallel()

	calls := &stubResolver{tenantByID: map[string]string{
		"call-same-tenant": "tenant-A",
	}}
	rbac := service.NewTopicRBACWithCallResolver(nil, nil, calls)

	err := rbac.Allow(t.Context(),
		rtapi.Claims{TenantID: "tenant-A", UserID: "u-1", Roles: []string{"operator"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "call-same-tenant"},
	)
	require.NoError(t, err)
}

// TestTopicRBAC_AllowFoldsCallResolverErrorIntoCrossTenant — a
// resolver-error path (not-found / DB error) must NOT distinguishably
// surface to the wire; the wire error is identical to a tenant
// mismatch so clients cannot probe call existence cross-tenant.
func TestTopicRBAC_AllowFoldsCallResolverErrorIntoCrossTenant(t *testing.T) {
	t.Parallel()

	calls := &stubResolver{tenantByID: map[string]string{}} // empty → all lookups fail
	rbac := service.NewTopicRBACWithCallResolver(nil, nil, calls)

	err := rbac.Allow(t.Context(),
		rtapi.Claims{TenantID: "tenant-A", UserID: "u-1", Roles: []string{"operator"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "call-x"},
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrCrossTenantSubscribe,
		"resolver error must fold into cross-tenant for wire indistinguishability")
}

// TestTopicRBAC_AllowSkipsCallCheckWhenCallResolverNil — preserves the
// degraded-boot path: when no CallResolver is wired (e.g. cmd/api
// hasn't connected the recording adapter), the cross-tenant CallID
// check is skipped entirely. Hub.Broadcast tenant-prefix filtering
// remains the upstream defence.
func TestTopicRBAC_AllowSkipsCallCheckWhenCallResolverNil(t *testing.T) {
	t.Parallel()

	rbac := service.NewTopicRBACWithCallResolver(nil, nil, nil)

	err := rbac.Allow(t.Context(),
		rtapi.Claims{TenantID: "tenant-A", UserID: "u-1", Roles: []string{"operator"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "any-call-id-from-any-tenant"},
	)
	require.NoError(t, err, "nil CallResolver must skip the cross-tenant check (degraded boot)")
}

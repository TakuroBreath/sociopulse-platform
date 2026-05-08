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
	"testing"

	"github.com/stretchr/testify/require"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

func TestRBAC_OperatorCannotSubscribeOperatorsState(t *testing.T) {
	t.Parallel()
	r := service.NewTopicRBAC()
	err := r.Allow(
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
		rtapi.Claims{UserID: "u1", Roles: []string{"supervisor", "operator"}},
		rtapi.TopicTrunksHealth,
		rtapi.SubscriptionFilter{},
	)
	require.ErrorIs(t, err, service.ErrTopicForbidden)

	// But TopicOperatorsState (admin + supervisor) → allowed via
	// supervisor role.
	err = r.Allow(
		rtapi.Claims{UserID: "u1", Roles: []string{"supervisor", "operator"}},
		rtapi.TopicOperatorsState,
		rtapi.SubscriptionFilter{},
	)
	require.NoError(t, err)
}

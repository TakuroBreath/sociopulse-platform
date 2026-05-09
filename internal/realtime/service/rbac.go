package service

import (
	"context"
	"fmt"
	"slices"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// Local sentinel aliases. The realtime api package owns the canonical
// error values; aliasing them here lets in-package consumers
// (Hub.Subscribe wiring, hubConnection facades) errors.Is(err,
// ErrTopicForbidden) without importing rtapi twice. Plan 09/10
// carry-forward — Plan 10 reviewer flagged the missing alias pattern.
var (
	ErrTopicForbidden       = rtapi.ErrTopicForbidden
	ErrUnknownTopic         = rtapi.ErrUnknownTopic
	ErrFilterRequired       = rtapi.ErrFilterRequired
	ErrCrossTenantSubscribe = rtapi.ErrCrossTenantSubscribe
)

// TopicRBAC enforces the per-topic role matrix, the per-topic filter
// requirements, AND the cross-tenant filter check (Plan 11.2 Task 4).
//
// The matrix is constructed once at NewTopicRBAC(WithResolvers) and
// never mutated; callers MUST treat the returned *TopicRBAC as
// immutable. Resolvers (when wired) are invoked synchronously on
// every Allow call — the production wiring uses cached resolvers
// (60s TTL + singleflight, see resolver_cache.go) so the per-frame
// cost is amortised.
type TopicRBAC struct {
	rules           map[rtapi.Topic]topicRule
	userResolver    rtapi.UserResolver    // optional; nil = skip cross-tenant on OperatorID
	projectResolver rtapi.ProjectResolver // optional; nil = skip cross-tenant on ProjectID
}

// topicRule is the policy attached to a single Topic.
//
// allowedRoles is the *union* of roles that may subscribe — a claim
// with any one of these roles passes the role check. Empty
// allowedRoles means "no role can subscribe" (we never declare such
// a rule; the field's emptiness is a defence against accidental
// matrix erasure).
//
// requireCallID flags topics where the SubscriptionFilter MUST carry
// a non-empty CallID (TopicCallEvents — per-call streams must be
// narrowed; broadcasting every call event to every subscriber would
// leak cross-tenant call metadata even within the tenant scope).
//
// selfOnly flags topics where the subscriber may only narrow to
// their own UserID. An empty OperatorID filter is treated as "self
// by default" so the operator UI can subscribe to its own
// notifications without echoing claims.UserID back into the filter.
type topicRule struct {
	allowedRoles  []string
	requireCallID bool
	selfOnly      bool
}

// NewTopicRBAC returns the canonical realtime RBAC matrix WITHOUT
// resolver wiring. Cross-tenant filter check is skipped — preserved
// for tests + degraded-boot paths where wiring real resolvers is
// undesirable. Production callers use NewTopicRBACWithResolvers.
//
// Adding a new Topic requires extending this map AND the
// rtapi.AllTopics slice — the topic registry test in
// internal/realtime/api/topics_test.go enforces the latter.
func NewTopicRBAC() *TopicRBAC {
	return &TopicRBAC{
		rules: defaultTopicRules(),
	}
}

// NewTopicRBACWithResolvers wires the resolvers used for the
// cross-tenant filter check. nil resolvers are allowed — the matching
// dimension simply skips the check. Production wiring (cmd/api +
// realtime.Module.Register) supplies both; tests typically supply
// stub resolvers for the dimension under test and nil for the
// other.
func NewTopicRBACWithResolvers(users rtapi.UserResolver, projects rtapi.ProjectResolver) *TopicRBAC {
	return &TopicRBAC{
		rules:           defaultTopicRules(),
		userResolver:    users,
		projectResolver: projects,
	}
}

// defaultTopicRules is the canonical rule set extracted into a
// helper so NewTopicRBAC and NewTopicRBACWithResolvers share the
// same map (DRY).
func defaultTopicRules() map[rtapi.Topic]topicRule {
	return map[rtapi.Topic]topicRule{
		rtapi.TopicOperatorsState: {allowedRoles: []string{"admin", "supervisor"}},
		rtapi.TopicDialerQueue:    {allowedRoles: []string{"admin", "supervisor"}},
		rtapi.TopicTrunksHealth:   {allowedRoles: []string{"admin"}},
		rtapi.TopicCallEvents: {
			allowedRoles:  []string{"operator", "admin", "supervisor"},
			requireCallID: true,
		},
		rtapi.TopicNotifications: {
			allowedRoles: []string{"operator", "admin", "supervisor"},
			selfOnly:     true,
		},
		rtapi.TopicForceCommands: {
			allowedRoles: []string{"operator", "admin", "supervisor"},
			selfOnly:     true,
		},
	}
}

// Allow reports whether claims may subscribe to topic with filter.
// Returns nil on success. On rejection returns one of:
//
//   - ErrUnknownTopic         — topic not in the matrix.
//   - ErrTopicForbidden       — role check failed OR selfOnly violation.
//   - ErrFilterRequired       — topic requires a CallID and the filter has none.
//   - ErrCrossTenantSubscribe — filter UUID resolves to a different
//     tenant than claims.TenantID, OR the resolver could not find
//     the UUID (folded together so the wire response can't probe
//     entity existence cross-tenant).
//
// All errors carry context via fmt.Errorf("%w: …") so the error
// chain preserves errors.Is matching at module boundaries.
//
// The signature gained ctx in Plan 11.2 Task 4 — resolvers are
// ctx-aware so a slow DB doesn't pin the subscribe path. Callers
// that previously passed nothing must now supply at least
// context.Background() (Hub passes the connection's run-ctx).
func (r *TopicRBAC) Allow(ctx context.Context, claims rtapi.Claims, topic rtapi.Topic, filter rtapi.SubscriptionFilter) error {
	rule, ok := r.rules[topic]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTopic, topic)
	}
	if !hasAnyRole(claims.Roles, rule.allowedRoles) {
		return fmt.Errorf("%w: roles=%v topic=%s", ErrTopicForbidden, claims.Roles, topic)
	}
	if rule.requireCallID && filter.CallID == "" {
		return fmt.Errorf("%w: topic=%s needs CallID", ErrFilterRequired, topic)
	}
	if err := r.checkSelfOnly(rule, claims, filter, topic); err != nil {
		return err
	}
	return r.checkCrossTenant(ctx, rule, claims, filter)
}

// checkSelfOnly enforces the selfOnly rule for the topic. Returns
// nil if the rule does not apply or the constraint is satisfied.
func (r *TopicRBAC) checkSelfOnly(rule topicRule, claims rtapi.Claims, filter rtapi.SubscriptionFilter, topic rtapi.Topic) error {
	if !rule.selfOnly {
		return nil
	}
	// An un-identified principal must never reach a self-scoped
	// feed. Hub.Connect rejects empty TenantID but not empty
	// UserID, so we enforce it here as the last line of defence.
	if claims.UserID == "" {
		return fmt.Errorf("%w: self-only topic=%s requires non-empty UserID", ErrTopicForbidden, topic)
	}
	if filter.OperatorID != "" && filter.OperatorID != claims.UserID {
		return fmt.Errorf("%w: self-only topic=%s", ErrTopicForbidden, topic)
	}
	return nil
}

// checkCrossTenant enforces the Plan 11.2 Task 4 cross-tenant filter
// check. Resolvers are optional — a nil resolver skips its
// dimension (preserves the Plan 11 behaviour for tests + degraded
// boot).
//
// selfOnly+matching-userID short-circuit: when the rule is selfOnly
// and filter.OperatorID equals claims.UserID, the resolver call is
// skipped — claims.TenantID already established the tenant
// relationship via the auth handshake.
//
// CallID cross-tenant check is intentionally NOT performed here.
// See Plan 11.2 plan, "Out of scope" — Plan 12 (recording metadata)
// introduces the CallStore.Get the third resolver would consume.
// Today, TopicCallEvents cross-tenant safety is enforced by the
// NATS subject prefix + Hub.Broadcast tenant filter (see Plan 11
// Task 3 + 4b doc).
func (r *TopicRBAC) checkCrossTenant(ctx context.Context, rule topicRule, claims rtapi.Claims, filter rtapi.SubscriptionFilter) error {
	if r.userResolver != nil && filter.OperatorID != "" {
		skipResolve := rule.selfOnly && filter.OperatorID == claims.UserID
		if !skipResolve {
			if err := verifyTenant(ctx, r.userResolver.Get, filter.OperatorID, claims.TenantID, "operator_id"); err != nil {
				return err
			}
		}
	}
	if r.projectResolver != nil && filter.ProjectID != "" {
		if err := verifyTenant(ctx, r.projectResolver.Get, filter.ProjectID, claims.TenantID, "project_id"); err != nil {
			return err
		}
	}
	return nil
}

// verifyTenant resolves id via getter and asserts its TenantID
// matches the subscriber's tenant. Both resolver errors and
// tenant mismatches are folded into ErrCrossTenantSubscribe so
// the wire response is indistinguishable — the client cannot
// probe entity existence cross-tenant. The inner err is
// stringified (not %w-wrapped) for the same reason: callers
// must NOT be able to errors.Is past ErrCrossTenantSubscribe to
// the underlying not-found / DB error.
func verifyTenant(
	ctx context.Context,
	getter func(context.Context, string) (rtapi.ResolvedTenant, error),
	id, wantTenant, label string,
) error {
	got, err := getter(ctx, id)
	if err != nil {
		// nolint:errorlint // intentional: fold not-found/DB error
		// into cross-tenant via %s so callers cannot errors.Is past
		// ErrCrossTenantSubscribe to the underlying error.
		return fmt.Errorf("%w: %s=%s: %s", ErrCrossTenantSubscribe, label, id, err.Error())
	}
	if got.TenantID != wantTenant {
		return fmt.Errorf("%w: %s=%s belongs to tenant=%s", ErrCrossTenantSubscribe, label, id, got.TenantID)
	}
	return nil
}

// hasAnyRole returns true if at least one element of have is also in
// want. O(have * want); both are tiny in practice (≤4 entries each)
// so a hashset would be over-engineering.
func hasAnyRole(have, want []string) bool {
	if len(want) == 0 {
		return false
	}
	for _, h := range have {
		if slices.Contains(want, h) {
			return true
		}
	}
	return false
}

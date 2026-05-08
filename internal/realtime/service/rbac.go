package service

import (
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
	ErrTopicForbidden = rtapi.ErrTopicForbidden
	ErrUnknownTopic   = rtapi.ErrUnknownTopic
	ErrFilterRequired = rtapi.ErrFilterRequired
)

// TopicRBAC enforces the per-topic role matrix and the per-topic
// filter requirements. It is a pure value (no goroutines, no I/O) so
// the Hub can hold it behind a *TopicRBAC field and consult it from
// any goroutine without locking.
//
// The matrix is constructed once at NewTopicRBAC and never mutated;
// callers MUST treat the returned *TopicRBAC as immutable. (We don't
// expose a setter.)
type TopicRBAC struct {
	rules map[rtapi.Topic]topicRule
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

// NewTopicRBAC returns the canonical realtime RBAC matrix. Pinning
// the matrix in code (rather than reading it from configuration)
// makes the security policy reviewable in PRs and impossible to
// silently widen at runtime.
//
// Adding a new Topic requires extending this map AND the
// rtapi.AllTopics slice — the topic registry test in
// internal/realtime/api/topics_test.go enforces the latter.
func NewTopicRBAC() *TopicRBAC {
	return &TopicRBAC{
		rules: map[rtapi.Topic]topicRule{
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
		},
	}
}

// Allow reports whether claims may subscribe to topic with filter.
// Returns nil on success. On rejection returns one of:
//
//   - ErrUnknownTopic   — topic not in the matrix.
//   - ErrTopicForbidden — claims has none of the topic's allowed roles,
//     OR the topic is selfOnly and filter.OperatorID names a different
//     user, OR the topic is selfOnly and claims.UserID is empty (an
//     un-identified principal cannot subscribe to a self-scoped feed
//     even with empty filter.OperatorID — the auth layer is supposed
//     to guarantee a non-empty UserID, and this is defence-in-depth
//     against an upstream wiring bug).
//   - ErrFilterRequired — topic requires a CallID and the filter has none.
//
// All errors carry context via fmt.Errorf("%w: …") so the error chain
// preserves errors.Is matching at module boundaries.
func (r *TopicRBAC) Allow(claims rtapi.Claims, topic rtapi.Topic, filter rtapi.SubscriptionFilter) error {
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
	if rule.selfOnly {
		// An un-identified principal must never reach a self-scoped
		// feed. Hub.Connect rejects empty TenantID but not empty
		// UserID, so we enforce it here as the last line of defence.
		if claims.UserID == "" {
			return fmt.Errorf("%w: self-only topic=%s requires non-empty UserID", ErrTopicForbidden, topic)
		}
		if filter.OperatorID != "" && filter.OperatorID != claims.UserID {
			return fmt.Errorf("%w: self-only topic=%s", ErrTopicForbidden, topic)
		}
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

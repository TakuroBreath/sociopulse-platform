package api

import "errors"

// Sentinel errors returned by realtime interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrAuthFailed is returned when a Frame.token cannot be validated.
	ErrAuthFailed = errors.New("realtime: auth failed")
	// ErrAuthRequired is returned when a non-auth frame arrives before authentication.
	ErrAuthRequired = errors.New("realtime: auth frame required")
	// ErrTopicForbidden is returned when the caller's role may not subscribe to the topic.
	ErrTopicForbidden = errors.New("realtime: topic not allowed for role")
	// ErrUnknownTopic is returned when the requested Topic is not in the catalog.
	ErrUnknownTopic = errors.New("realtime: unknown topic")
	// ErrFilterRequired is returned when a topic requires a SubscriptionFilter that wasn't provided.
	ErrFilterRequired = errors.New("realtime: subscription filter is required for this topic")
	// ErrCallNotActive is returned by ListenInService.Start when the call has already hung up.
	ErrCallNotActive = errors.New("realtime: call not active")
	// ErrListenerLimit is returned when the per-call listener cap is hit.
	ErrListenerLimit = errors.New("realtime: listener limit reached for call")
	// ErrConnectionClosed is returned by Connection methods invoked after Close.
	// Hub-side helpers and tests use errors.Is to discriminate from transient
	// network errors that may legitimately produce a write failure.
	ErrConnectionClosed = errors.New("realtime: connection closed")
	// ErrSlowConsumer is the sentinel emitted (best-effort, via metrics) when a
	// frame is dropped because the per-connection send buffer was full. Returned
	// by Send-style helpers that surface drops back to the caller; the public
	// Connection.Send swallows it and only increments the dropped counter.
	ErrSlowConsumer = errors.New("realtime: slow consumer (frame dropped)")
	// ErrCrossTenantSubscribe is returned when TopicRBAC.Allow detects
	// that a SubscriptionFilter UUID (OperatorID, ProjectID) belongs
	// to a tenant other than the subscriber's claims.TenantID. Defence-
	// in-depth — Hub.Broadcast already filters by tenant, but a
	// cross-tenant subscribe attempt is a security signal we want to
	// surface (the subscriber should never have observed the foreign
	// UUID in the first place).
	ErrCrossTenantSubscribe = errors.New("realtime: cross-tenant subscription denied")
)

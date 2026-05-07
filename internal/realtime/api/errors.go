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
)

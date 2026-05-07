package api

import "errors"

// Sentinel errors returned by dialer interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrInvalidTransition is returned when an FSM event is not permitted from the current state.
	ErrInvalidTransition = errors.New("dialer: invalid FSM transition")
	// ErrUnknownState is returned when a stored state value cannot be decoded (corrupt row).
	ErrUnknownState = errors.New("dialer: unknown state")
	// ErrQueueEmpty is returned when CallQueue.PickNext finds no eligible item.
	ErrQueueEmpty = errors.New("dialer: queue empty")
	// ErrDuplicateInQueue is returned by EnqueueRespondent when the respondent is already queued.
	ErrDuplicateInQueue = errors.New("dialer: respondent already in queue")
	// ErrAllNodesFull is returned when every FreeSWITCH node is at capacity.
	ErrAllNodesFull = errors.New("dialer: all FreeSWITCH nodes at capacity")
	// ErrOutsideWorkingHours is returned by WorkingHoursChecker when the local time is outside permitted dialing hours.
	ErrOutsideWorkingHours = errors.New("dialer: outside working hours for region")
	// ErrThrottled is returned when the per-tenant rate limit kicks in.
	ErrThrottled = errors.New("dialer: rate-limit throttled")
	// ErrTenantMismatch is returned when an operation crosses tenants (defence in depth).
	ErrTenantMismatch = errors.New("dialer: tenant mismatch")
)

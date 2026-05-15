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
	// ErrConflict is returned when a concurrent transition committed first
	// (optimistic-concurrency CAS lost). The caller MAY retry: load the
	// current snapshot and re-apply the event if it's still meaningful in
	// the new state. Implementations: dialer/fsm Machine surfaces this on
	// any state-changing call (StartShift / EndShift / GoPause / ... /
	// Force) when the hash version has advanced between load and CAS.
	ErrConflict = errors.New("dialer: optimistic-concurrency conflict")
	// ErrCallNotFound is returned by CallTenantResolver.LookupCallTenant
	// when no row in the calls table matches the requested call_id.
	// The transport-layer adapter for tenant.RequireSameTenant folds
	// this into pkg/middleware/tenant.ErrNotFound so the wire response
	// is a 404 with no body, indistinguishable from a "wrong tenant"
	// mismatch (existence-probe defence). Plan 21 Task 3.
	ErrCallNotFound = errors.New("dialer: call not found")
)

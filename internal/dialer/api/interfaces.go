package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ChannelEventHandler is invoked once per telephony event the dialer subscribes to.
type ChannelEventHandler func(ctx context.Context, evt ChannelEvent) error

// OperatorFSM is the per-operator state-machine surface. cmd/api translates
// HTTP requests on /api/operator/* into FSM calls; each call returns the
// new Snapshot so the HTTP handler can render the operator UI deterministically.
type OperatorFSM interface {
	// StartShift transitions offline → ready and binds the operator to a project.
	StartShift(ctx context.Context, req StartShiftRequest) (Snapshot, error)
	// EndShift transitions any state → offline at end-of-shift.
	EndShift(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)
	// GoReady transitions pause → ready (or status / verify → ready).
	GoReady(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)
	// GoPause transitions any non-call state → pause with a reason.
	GoPause(ctx context.Context, req GoPauseRequest) (Snapshot, error)
	// Resume is an alias for GoReady from pause specifically.
	Resume(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)
	// RecordCallStarted transitions dialing → call.
	RecordCallStarted(ctx context.Context, req CallStartedRequest) (Snapshot, error)
	// RecordCallEnded transitions call → status (or dialing → ready on no-answer).
	RecordCallEnded(ctx context.Context, req CallEndedRequest) (Snapshot, error)
	// SubmitStatus transitions status → ready with the call disposition.
	SubmitStatus(ctx context.Context, req SubmitStatusRequest) (Snapshot, error)
	// GoVerify transitions status → verify (supervisor recheck).
	GoVerify(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)
	// VerifyDone transitions verify → ready.
	VerifyDone(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)
	// GetState returns the current Snapshot without performing a transition.
	GetState(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)
	// Force is the supervisor escape hatch — sets State to target with a reason.
	// reason is a typed enum so the Prometheus dialer_fsm_force_total{reason}
	// label cardinality stays bounded; unrecognised values are bucketed under
	// ForceReasonOther by the implementation.
	Force(ctx context.Context, tenantID, operatorID uuid.UUID, target State, reason ForceReason) (Snapshot, error)
}

// CallQueue is the Redis ZSET surface used by the dialer worker loop.
type CallQueue interface {
	// EnqueueRespondent adds a respondent to the queue.
	// Returns ok=false (without error) when the respondent is already queued.
	EnqueueRespondent(ctx context.Context, req EnqueueRequest) (ok bool, err error)
	// PickNext atomically pops the highest-priority eligible item.
	// Returns ErrQueueEmpty when nothing is ready.
	PickNext(ctx context.Context, tenantID, projectID uuid.UUID) (QueueItem, error)
	// Requeue re-inserts an item with delay (used for retries / no-answer).
	Requeue(ctx context.Context, item QueueItem, delay time.Duration) error
	// Size returns the current number of items in the project queue.
	Size(ctx context.Context, tenantID, projectID uuid.UUID) (int64, error)
	// Remove deletes the respondent's entry from the queue (used on DNC, deletion).
	Remove(ctx context.Context, tenantID, projectID, respondentID uuid.UUID) error
}

// RDDGenerator generates Random Digit Dialing phone numbers under the
// project's quota plan. The implementation honours DNC and per-tenant rate caps.
type RDDGenerator interface {
	// Generate returns up to req.N synthesised respondents.
	// Implementations are best-effort: Generated may be < N when quotas are full.
	Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
}

// Router is the dialer's NATS abstraction in front of telephony commands.
// Concrete adapter wraps telephony/api.CommandPublisher and EventConsumer.
type Router interface {
	// Dial places an outbound call via the telephony bridge.
	Dial(ctx context.Context, req DialRequest) error
	// Hangup ends a call via the telephony bridge.
	Hangup(ctx context.Context, callID uuid.UUID, reason string) error
	// Subscribe attaches h to the per-tenant telephony event stream.
	Subscribe(ctx context.Context, tenantID uuid.UUID, h ChannelEventHandler) (unsubscribe func(), err error)
}

// LineCapacityTracker enforces the per-FS-node 60-channel cap. Mirrors the
// telephony.LineCapacityTracker shape but lives in dialer because the dialer
// is the only consumer.
type LineCapacityTracker interface {
	// Acquire reserves one channel on a healthy node and returns its name.
	Acquire(ctx context.Context) (node string, err error)
	// Release returns one channel to the pool.
	Release(ctx context.Context, node string) error
	// Stats returns per-node concurrency counters.
	Stats(ctx context.Context) (map[string]int64, error)
}

// WorkingHoursChecker enforces the per-tenant + per-region permitted dialing window.
type WorkingHoursChecker interface {
	// IsAllowed reports whether dialing is currently permitted for the region at time at.
	IsAllowed(ctx context.Context, tenantID uuid.UUID, region string, at time.Time) (bool, error)
	// NextAllowed returns the next time after at when dialing becomes permitted.
	NextAllowed(ctx context.Context, tenantID uuid.UUID, region string, at time.Time) (time.Time, error)
}

// RetryOrchestrator scans the retry table for mature pending retries and
// re-enqueues them. cmd/worker constructs one and calls Run; it blocks
// until ctx is cancelled.
type RetryOrchestrator interface {
	// Run blocks until ctx cancels.
	Run(ctx context.Context) error
}

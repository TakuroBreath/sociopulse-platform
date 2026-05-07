package api

import "context"

// ReportRenderer turns a (kind, params, window) tuple into an exported file.
// The synchronous path; large windows must be enqueued via JobQueue instead.
type ReportRenderer interface {
	// Render produces the report file as raw bytes plus filename+MIME+sha256.
	Render(ctx context.Context, in RenderInput) (RenderResult, error)
}

// ReportRunner wraps Render with HTTP-layer policy (size caps, audit logging).
// For larger windows the HTTP handler enqueues a Job instead.
type ReportRunner interface {
	// Run executes a synchronous report. For windows the runner judges too
	// large the implementation may return ErrTooLarge to nudge the caller
	// to use the async JobQueue path.
	Run(ctx context.Context, in RunInput) (RunResult, error)
}

// JobQueue is the asynq-backed job tracker.
type JobQueue interface {
	// Enqueue stores a job and queues an asynq task. Returns the ticket
	// the caller can poll via Get.
	Enqueue(ctx context.Context, in JobInput) (JobTicket, error)
	// Get returns a job by ID, or ErrJobNotFound.
	Get(ctx context.Context, jobID string) (Job, error)
	// List returns jobs matching f. The second return is the next cursor;
	// empty string means no further pages.
	List(ctx context.Context, f ListJobsFilter) ([]Job, string, error)
	// Cancel marks a queued / running job as canceled.
	Cancel(ctx context.Context, jobID string) error
}

// JobConsumer is the asynq worker side. cmd/worker constructs one and calls Run;
// it blocks until ctx is cancelled.
type JobConsumer interface {
	// Run blocks until ctx cancels.
	Run(ctx context.Context) error
}

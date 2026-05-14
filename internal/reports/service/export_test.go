package service

import (
	"context"
	"time"

	"github.com/hibiken/asynq"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// RenderForTest exposes the unexported renderByKind dispatcher to the
// external service_test package so tests can exercise the
// KindCustom → project_summary mapping (which Run() never reaches
// because Custom always trips ErrAsyncRequired at the threshold gate).
//
// This is the standard Go pattern for test-only exports: only the
// service_test package (file suffix _test.go) sees this symbol; it does
// not pollute the public API.
func RenderForTest(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (reportsapi.RenderResult, error) {
	return renderByKind(ctx, ana, in)
}

// SetCancelFlipForTest swaps the Queue's MarkCanceledTx wrapper for a
// caller-supplied fake. Used by queue_test.go to drive the ErrStaleSkip
// idempotency branch without a real Postgres transaction.
func SetCancelFlipForTest(q *Queue, fn func(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time) error) {
	q.cancelFlip = fn
}

// SetConsumerFlipsForTest swaps the Consumer's Mark*Tx wrappers for
// caller-supplied fakes. Used by consumer_test.go to drive the
// ErrStaleSkip / failure branches without a real Postgres transaction.
// Pass nil for any flip to leave its existing wiring (e.g., production
// default reportstore.Mark*Tx) in place.
func SetConsumerFlipsForTest(
	c *Consumer,
	running func(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time) error,
	succeeded func(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time, bytesSize int64, filename, url string) error,
	failed func(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time, errMsg string) error,
) {
	if running != nil {
		c.runningFlip = running
	}
	if succeeded != nil {
		c.succeededFlip = succeeded
	}
	if failed != nil {
		c.failedFlip = failed
	}
}

// HandleJobRunForTest exposes the unexported handleJobRun to the
// service_test package. consumer_test.go drives it directly, bypassing
// the asynq.Server lifecycle so the unit tests don't need Redis.
func HandleJobRunForTest(c *Consumer, ctx context.Context, task *asynq.Task) error {
	return c.handleJobRun(ctx, task)
}

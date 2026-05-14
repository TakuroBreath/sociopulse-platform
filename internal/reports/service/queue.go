package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	reportstore "github.com/sociopulse/platform/internal/reports/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// AsynqEnqueuer is the asynq.Client surface Queue needs (test seam).
// *asynq.Client satisfies it via EnqueueContext.
type AsynqEnqueuer interface {
	EnqueueContext(ctx context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

// QueueStore is the reports_jobs persistence surface Queue needs.
// *reportstore.PG satisfies it via Create / Get / List / SelectTenantByJobID.
//
// Note the tenantID parameter on Get / List: the underlying store opens
// its own pool.WithTenant scope, so the Queue does not wrap the call in
// an outer WithTenant — that would double-set app.tenant_id for no
// gain. Queue passes the caller-resolved tenantID through.
type QueueStore interface {
	Create(ctx context.Context, in reportstore.CreateInput) error
	Get(ctx context.Context, tenantID uuid.UUID, jobID string) (reportsapi.Job, error)
	List(ctx context.Context, tenantID uuid.UUID, f reportsapi.ListJobsFilter) ([]reportsapi.Job, string, error)
	SelectTenantByJobID(ctx context.Context, jobID string) (uuid.UUID, error)
}

// QueuePool is the postgres.Pool surface Queue needs. Only Cancel uses
// it directly — Cancel resolves the tenant via store.SelectTenantByJobID
// (which runs under BypassRLS internally) and then re-enters WithTenant
// to run the MarkCanceledTx state-flip atomically.
//
// *postgres.Pool satisfies this interface; tests inject a fake that
// invokes fn with a zero postgres.Tx so the MarkCanceledTx call site is
// observable without spinning up Postgres (the *Tx state-flip is then
// exercised separately under the integration build tag).
type QueuePool interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
}

// cancelFlipFn is the state-flip closure Queue.Cancel calls inside
// pool.WithTenant. Production wires reportstore.MarkCanceledTx; tests
// substitute a fake so the ErrStaleSkip-swallowing branch in Cancel
// can be exercised without a real Postgres tx.
type cancelFlipFn = func(ctx context.Context, tx postgres.Tx, jobID string, ts time.Time) error

// Queue is the asynq-backed implementation of reportsapi.JobQueue.
//
//   - Enqueue:  validates JobInput → asynq.EnqueueContext (the asynq
//     task id becomes the reports_jobs primary key) → store.Create
//     (which opens its own pool.WithTenant scope). If store.Create
//     fails AFTER a successful enqueue, the task becomes orphaned in
//     Redis. The Consumer picks it up, finds no row, and acks via
//     asynq.SkipRetry (no-op).
//
//   - Get:      resolves the tenant via store.SelectTenantByJobID
//     (BypassRLS), then store.Get(tenantID, jobID). HTTP layer guards
//     cross-tenant access via pkg/middleware/tenant.RequireSameTenant
//     before invoking — Get itself returns the row honestly because
//     the resolver returns ErrJobNotFound for a missing id.
//
//   - List:     takes the caller-supplied f.TenantID and delegates to
//     store.List, which opens its own WithTenant scope. f.TenantID
//     == uuid.Nil is rejected with ErrInvalidParams.
//
//   - Cancel:   resolves tenant via SelectTenantByJobID, then
//     pool.WithTenant + MarkCanceledTx. Idempotent on already-terminal
//     rows via reportstore.ErrStaleSkip (swallowed → success).
type Queue struct {
	store      QueueStore
	pool       QueuePool
	enq        AsynqEnqueuer
	queueName  string // "reports"
	now        func() time.Time
	cancelFlip cancelFlipFn
}

// NewQueue constructs a Queue. queueName defaults to "reports" when
// empty (the project's per-module asynq queue convention). cancelFlip
// defaults to reportstore.MarkCanceledTx and is only swapped out in
// unit tests via the exported helper below.
func NewQueue(store QueueStore, pool QueuePool, enq AsynqEnqueuer, queueName string) *Queue {
	if queueName == "" {
		queueName = "reports"
	}
	return &Queue{
		store:      store,
		pool:       pool,
		enq:        enq,
		queueName:  queueName,
		now:        time.Now,
		cancelFlip: reportstore.MarkCanceledTx,
	}
}

// Compile-time guard: Queue implements the reports API contract.
var _ reportsapi.JobQueue = (*Queue)(nil)

// Enqueue validates the input, calls asynq.EnqueueContext to obtain a
// stable task id, then inserts the reports_jobs row with that id inside
// the store's per-tenant scope. On row-insert failure the task becomes
// orphaned in Redis; the Consumer's MarkRunningTx call then returns
// ErrStaleSkip-equivalent (no row → ErrJobNotFound) and acks the task.
//
// Returns:
//   - reportsapi.ErrInvalidParams on missing tenant_id.
//   - analyticsapi.ErrInvalidWindow bare on bad window (Plan 13.2 lesson #3).
//   - reportsapi.ErrUnknownKind / ErrUnsupportedFmt on bad enums.
//   - wrapped errors for asynq / store failures.
func (q *Queue) Enqueue(ctx context.Context, in reportsapi.JobInput) (reportsapi.JobTicket, error) {
	if err := ctx.Err(); err != nil {
		return reportsapi.JobTicket{}, err
	}
	if in.TenantID == uuid.Nil {
		return reportsapi.JobTicket{}, fmt.Errorf("queue.Enqueue: %w: tenant_id missing", reportsapi.ErrInvalidParams)
	}
	if err := in.Window.Validate(); err != nil {
		return reportsapi.JobTicket{}, err // bare ErrInvalidWindow per Plan 13.2 lesson #3
	}
	if !knownKind(in.Kind) {
		return reportsapi.JobTicket{}, fmt.Errorf("queue.Enqueue: %w: %s", reportsapi.ErrUnknownKind, in.Kind)
	}
	if !knownFormat(in.Format) {
		return reportsapi.JobTicket{}, fmt.Errorf("queue.Enqueue: %w: %s", reportsapi.ErrUnsupportedFmt, in.Format)
	}

	payload, err := json.Marshal(in)
	if err != nil {
		return reportsapi.JobTicket{}, fmt.Errorf("queue.Enqueue: marshal: %w", err)
	}
	task := asynq.NewTask(reportsapi.TaskJobRun, payload)
	info, err := q.enq.EnqueueContext(ctx, task, asynq.Queue(q.queueName))
	if err != nil {
		return reportsapi.JobTicket{}, fmt.Errorf("queue.Enqueue: asynq: %w", err)
	}

	if err := q.store.Create(ctx, reportstore.CreateInput{
		ID:           info.ID,
		TenantID:     in.TenantID,
		Kind:         in.Kind,
		Format:       in.Format,
		Params:       in.Params,
		WindowFrom:   in.Window.From,
		WindowTo:     in.Window.To,
		CreatedBy:    in.ActorID,
		NotifyUserID: in.NotifyUserID,
	}); err != nil {
		// Task is orphaned in Redis; the Consumer's MarkRunningTx will
		// find no row (treated as a stale-skip) and ack.
		return reportsapi.JobTicket{}, fmt.Errorf("queue.Enqueue: persist row: %w", err)
	}

	return reportsapi.JobTicket{JobID: info.ID, QueuedAt: q.now().UTC()}, nil
}

// Get reads a job by id. Resolves the owning tenant first via
// SelectTenantByJobID (BypassRLS), then delegates to store.Get under
// that tenant's WithTenant scope.
//
// Returns reportsapi.ErrJobNotFound (wrapped) when the row is missing.
//
// HTTP layer must verify the returned Job.TenantID matches the caller's
// tenant before exposing it — the recommended pattern is to mount the
// Task 7 handlers behind pkg/middleware/tenant.RequireSameTenant whose
// resolver calls SelectTenantByJobID; the handler then trusts the
// returned Job's TenantID.
func (q *Queue) Get(ctx context.Context, jobID string) (reportsapi.Job, error) {
	if err := ctx.Err(); err != nil {
		return reportsapi.Job{}, err
	}
	tenantID, err := q.store.SelectTenantByJobID(ctx, jobID)
	if err != nil {
		return reportsapi.Job{}, err // ErrJobNotFound wrap survives
	}
	return q.store.Get(ctx, tenantID, jobID)
}

// List returns jobs matching f for f.TenantID. Caller MUST set
// f.TenantID; uuid.Nil is rejected with ErrInvalidParams.
func (q *Queue) List(ctx context.Context, f reportsapi.ListJobsFilter) ([]reportsapi.Job, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if f.TenantID == uuid.Nil {
		return nil, "", fmt.Errorf("queue.List: %w: tenant_id missing", reportsapi.ErrInvalidParams)
	}
	return q.store.List(ctx, f.TenantID, f)
}

// Cancel marks the job canceled. Idempotent on already-terminal rows
// via reportstore.ErrStaleSkip (swallowed → success).
//
// Returns reportsapi.ErrJobNotFound when the row is missing — Cancel
// cannot recover from a missing row because there is no tenant to
// re-enter WithTenant against.
func (q *Queue) Cancel(ctx context.Context, jobID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tenantID, err := q.store.SelectTenantByJobID(ctx, jobID)
	if err != nil {
		return err // ErrJobNotFound wrap survives
	}
	return q.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		flipErr := q.cancelFlip(ctx, tx, jobID, q.now().UTC())
		if errors.Is(flipErr, reportstore.ErrStaleSkip) {
			return nil // already terminal — idempotent
		}
		return flipErr
	})
}

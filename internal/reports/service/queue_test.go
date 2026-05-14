package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	reportsvc "github.com/sociopulse/platform/internal/reports/service"
	reportstore "github.com/sociopulse/platform/internal/reports/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// fakeAsynqEnqueuer records every EnqueueContext call and returns a
// caller-supplied TaskInfo (default: ID="task-1", Queue="reports") so
// tests can assert the asynq.Queue option carried through.
type fakeAsynqEnqueuer struct {
	called   int
	lastTask *asynq.Task
	lastOpts []asynq.Option
	nextInfo *asynq.TaskInfo
	nextErr  error
}

func (f *fakeAsynqEnqueuer) EnqueueContext(_ context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	f.called++
	f.lastTask = task
	f.lastOpts = opts
	if f.nextErr != nil {
		return nil, f.nextErr
	}
	if f.nextInfo != nil {
		return f.nextInfo, nil
	}
	return &asynq.TaskInfo{ID: "task-1", Queue: "reports", Type: task.Type(), Payload: task.Payload()}, nil
}

// Compile-time guard.
var _ reportsvc.AsynqEnqueuer = (*fakeAsynqEnqueuer)(nil)

// fakeQueueStore is a tiny in-memory QueueStore. Get / List read from a
// jobs map; Create captures the input; SelectTenantByJobID maps jobID
// to tenant. The nextErr hook lets tests inject store-side failures.
type fakeQueueStore struct {
	jobs        map[string]reportsapi.Job
	tenants     map[string]uuid.UUID
	createCalls []reportstore.CreateInput
	listCalls   []listCall

	createErr  error
	getErr     error
	listErr    error
	resolveErr error
}

type listCall struct {
	tenantID uuid.UUID
	filter   reportsapi.ListJobsFilter
}

func newFakeQueueStore() *fakeQueueStore {
	return &fakeQueueStore{
		jobs:    make(map[string]reportsapi.Job),
		tenants: make(map[string]uuid.UUID),
	}
}

func (f *fakeQueueStore) Create(_ context.Context, in reportstore.CreateInput) error {
	f.createCalls = append(f.createCalls, in)
	if f.createErr != nil {
		return f.createErr
	}
	f.jobs[in.ID] = reportsapi.Job{
		ID:        in.ID,
		TenantID:  in.TenantID,
		Kind:      in.Kind,
		Format:    in.Format,
		Params:    in.Params,
		Window:    analyticsapi.Window{From: in.WindowFrom, To: in.WindowTo},
		State:     reportsapi.JobQueued,
		CreatedBy: in.CreatedBy,
		CreatedAt: time.Now().UTC(),
	}
	f.tenants[in.ID] = in.TenantID
	return nil
}

func (f *fakeQueueStore) Get(_ context.Context, tenantID uuid.UUID, jobID string) (reportsapi.Job, error) {
	if f.getErr != nil {
		return reportsapi.Job{}, f.getErr
	}
	j, ok := f.jobs[jobID]
	if !ok || j.TenantID != tenantID {
		return reportsapi.Job{}, reportsapi.ErrJobNotFound
	}
	return j, nil
}

func (f *fakeQueueStore) List(_ context.Context, tenantID uuid.UUID, filter reportsapi.ListJobsFilter) ([]reportsapi.Job, string, error) {
	f.listCalls = append(f.listCalls, listCall{tenantID: tenantID, filter: filter})
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	out := make([]reportsapi.Job, 0, len(f.jobs))
	for _, j := range f.jobs {
		if j.TenantID == tenantID {
			out = append(out, j)
		}
	}
	return out, "", nil
}

func (f *fakeQueueStore) SelectTenantByJobID(_ context.Context, jobID string) (uuid.UUID, error) {
	if f.resolveErr != nil {
		return uuid.Nil, f.resolveErr
	}
	tid, ok := f.tenants[jobID]
	if !ok {
		return uuid.Nil, reportsapi.ErrJobNotFound
	}
	return tid, nil
}

// Compile-time guard.
var _ reportsvc.QueueStore = (*fakeQueueStore)(nil)

// fakeQueuePool is the QueuePool surface for Queue.Cancel. Invokes the
// caller's fn with a zero postgres.Tx — works because Queue.Cancel's
// closure calls q.cancelFlip (a function pointer the test swaps via
// SetCancelFlipForTest) instead of the real MarkCanceledTx, so the
// nil-tx never reaches a real pgx call.
type fakeQueuePool struct {
	tenants []uuid.UUID
}

func (f *fakeQueuePool) WithTenant(_ context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error {
	f.tenants = append(f.tenants, tenantID)
	return fn(postgres.Tx{})
}

// Compile-time guard.
var _ reportsvc.QueuePool = (*fakeQueuePool)(nil)

// ─── shared fixtures ─────────────────────────────────────────────────────────

var (
	queueTenant  = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	queueActor   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	queueProject = uuid.MustParse("33333333-3333-3333-3333-333333333333")
)

func newJobInput() reportsapi.JobInput {
	return reportsapi.JobInput{
		RenderInput: reportsapi.RenderInput{
			Kind:     reportsapi.KindOperatorEfficiency,
			Format:   reportsapi.FormatXLSX,
			Params:   map[string]any{"project_id": queueProject.String()},
			Window:   analyticsapi.Window{From: dataFrom, To: dataTo},
			TenantID: queueTenant,
			ActorID:  queueActor,
		},
		NotifyUserID: queueActor,
	}
}

// ─── Enqueue ─────────────────────────────────────────────────────────────────

func TestQueue_Enqueue_Happy(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "")

	ticket, err := q.Enqueue(context.Background(), newJobInput())
	require.NoError(t, err)
	require.Equal(t, "task-1", ticket.JobID)
	require.False(t, ticket.QueuedAt.IsZero())

	// asynq was called exactly once with the right task type and queue option.
	require.Equal(t, 1, enq.called)
	require.NotNil(t, enq.lastTask)
	require.Equal(t, reportsapi.TaskJobRun, enq.lastTask.Type())
	require.NotEmpty(t, enq.lastTask.Payload(), "payload must be JSON-marshalled JobInput")

	// store.Create was called with the TaskInfo.ID as PK.
	require.Len(t, store.createCalls, 1)
	require.Equal(t, "task-1", store.createCalls[0].ID)
	require.Equal(t, queueTenant, store.createCalls[0].TenantID)
}

func TestQueue_Enqueue_RejectsZeroTenant(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	in := newJobInput()
	in.TenantID = uuid.Nil
	_, err := q.Enqueue(context.Background(), in)
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
	require.Equal(t, 0, enq.called, "asynq must not be called when input invalid")
	require.Empty(t, store.createCalls)
}

func TestQueue_Enqueue_RejectsInvalidWindow(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	in := newJobInput()
	// Invert From/To — Window.Validate rejects.
	in.Window = analyticsapi.Window{From: dataTo, To: dataFrom}
	_, err := q.Enqueue(context.Background(), in)
	require.ErrorIs(t, err, analyticsapi.ErrInvalidWindow,
		"window validation returns bare ErrInvalidWindow per Plan 13.2 lesson #3")
	require.Equal(t, 0, enq.called)
}

func TestQueue_Enqueue_RejectsUnknownKind(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	in := newJobInput()
	in.Kind = "bogus-kind"
	_, err := q.Enqueue(context.Background(), in)
	require.ErrorIs(t, err, reportsapi.ErrUnknownKind)
	require.Equal(t, 0, enq.called)
}

func TestQueue_Enqueue_RejectsUnsupportedFormat(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	in := newJobInput()
	in.Format = "tarball"
	_, err := q.Enqueue(context.Background(), in)
	require.ErrorIs(t, err, reportsapi.ErrUnsupportedFmt)
	require.Equal(t, 0, enq.called)
}

func TestQueue_Enqueue_PropagatesAsynqError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("redis-down")
	enq := &fakeAsynqEnqueuer{nextErr: sentinel}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	_, err := q.Enqueue(context.Background(), newJobInput())
	require.ErrorIs(t, err, sentinel, "asynq error must survive %w wrap")
	require.Empty(t, store.createCalls, "store.Create must not run after asynq failed")
}

func TestQueue_Enqueue_PropagatesStoreCreateError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("rls-rejected")
	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	store.createErr = sentinel
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	_, err := q.Enqueue(context.Background(), newJobInput())
	require.ErrorIs(t, err, sentinel, "store error must survive %w wrap")
	require.Equal(t, 1, enq.called, "asynq runs BEFORE store.Create — task becomes orphaned, Consumer no-ops")
}

func TestQueue_Enqueue_DefaultsQueueNameToReports(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	// Empty queue name → constructor must default to "reports".
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "")

	_, err := q.Enqueue(context.Background(), newJobInput())
	require.NoError(t, err)
	require.Equal(t, 1, enq.called)
	require.NotEmpty(t, enq.lastOpts, "the asynq.Queue option must be passed")
}

// ─── Get ─────────────────────────────────────────────────────────────────────

func TestQueue_Get_Happy(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	_, err := q.Enqueue(context.Background(), newJobInput())
	require.NoError(t, err)

	got, err := q.Get(context.Background(), "task-1")
	require.NoError(t, err)
	require.Equal(t, "task-1", got.ID)
	require.Equal(t, queueTenant, got.TenantID)
	require.Equal(t, reportsapi.JobQueued, got.State)
}

func TestQueue_Get_NotFound(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	_, err := q.Get(context.Background(), "missing-id")
	require.ErrorIs(t, err, reportsapi.ErrJobNotFound)
}

// ─── List ────────────────────────────────────────────────────────────────────

func TestQueue_List_RejectsZeroTenant(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	_, _, err := q.List(context.Background(), reportsapi.ListJobsFilter{
		TenantID: uuid.Nil,
		Limit:    10,
	})
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
	require.Empty(t, store.listCalls, "store.List must not run when tenant missing")
}

func TestQueue_List_Happy_ForwardsFilterAndTenant(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	q := reportsvc.NewQueue(store, &fakeQueuePool{}, enq, "reports")

	_, err := q.Enqueue(context.Background(), newJobInput())
	require.NoError(t, err)

	st := reportsapi.JobQueued
	filter := reportsapi.ListJobsFilter{
		TenantID: queueTenant,
		State:    &st,
		Limit:    10,
	}
	got, _, err := q.List(context.Background(), filter)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "task-1", got[0].ID)

	// Filter was forwarded verbatim to the store.
	require.Len(t, store.listCalls, 1)
	require.Equal(t, queueTenant, store.listCalls[0].tenantID)
	require.Equal(t, queueTenant, store.listCalls[0].filter.TenantID)
	require.Equal(t, &st, store.listCalls[0].filter.State)
}

// fakeCancelFlip is a recording substitute for reportstore.MarkCanceledTx
// injected via SetCancelFlipForTest. Tests configure nextErr to drive
// the Queue's ErrStaleSkip-swallowing / propagation branches.
type fakeCancelFlip struct {
	calls   int
	nextErr error
}

func (f *fakeCancelFlip) fn(_ context.Context, _ postgres.Tx, _ string, _ time.Time) error {
	f.calls++
	return f.nextErr
}

// ─── Cancel ──────────────────────────────────────────────────────────────────

func TestQueue_Cancel_Happy(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	pool := &fakeQueuePool{}
	q := reportsvc.NewQueue(store, pool, enq, "reports")

	_, err := q.Enqueue(context.Background(), newJobInput())
	require.NoError(t, err)

	flip := &fakeCancelFlip{}
	reportsvc.SetCancelFlipForTest(q, flip.fn)

	err = q.Cancel(context.Background(), "task-1")
	require.NoError(t, err)
	require.Equal(t, 1, flip.calls, "MarkCanceledTx must be invoked once")
	require.Len(t, pool.tenants, 1)
	require.Equal(t, queueTenant, pool.tenants[0], "Cancel must enter WithTenant on the row's tenant")
}

func TestQueue_Cancel_IdempotentOnStaleSkip(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	pool := &fakeQueuePool{}
	q := reportsvc.NewQueue(store, pool, enq, "reports")

	_, err := q.Enqueue(context.Background(), newJobInput())
	require.NoError(t, err)

	// Simulate "already terminal" — MarkCanceledTx returns ErrStaleSkip.
	flip := &fakeCancelFlip{nextErr: reportstore.ErrStaleSkip}
	reportsvc.SetCancelFlipForTest(q, flip.fn)

	err = q.Cancel(context.Background(), "task-1")
	require.NoError(t, err, "Cancel must treat ErrStaleSkip as success (idempotent)")
}

func TestQueue_Cancel_PropagatesNonStaleError(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	pool := &fakeQueuePool{}
	q := reportsvc.NewQueue(store, pool, enq, "reports")

	_, err := q.Enqueue(context.Background(), newJobInput())
	require.NoError(t, err)

	sentinel := errors.New("db-blew-up")
	flip := &fakeCancelFlip{nextErr: sentinel}
	reportsvc.SetCancelFlipForTest(q, flip.fn)

	err = q.Cancel(context.Background(), "task-1")
	require.ErrorIs(t, err, sentinel, "non-stale errors must propagate")
}

func TestQueue_Cancel_NotFound(t *testing.T) {
	t.Parallel()

	enq := &fakeAsynqEnqueuer{}
	store := newFakeQueueStore()
	pool := &fakeQueuePool{}
	q := reportsvc.NewQueue(store, pool, enq, "reports")

	// No Enqueue first — SelectTenantByJobID returns ErrJobNotFound.
	err := q.Cancel(context.Background(), "no-such-job")
	require.ErrorIs(t, err, reportsapi.ErrJobNotFound)
	require.Empty(t, pool.tenants, "Cancel must not enter WithTenant when resolve fails")
}

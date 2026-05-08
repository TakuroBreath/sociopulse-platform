package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
)

// fakeEventBusPublisher captures Publish calls without speaking to NATS.
type fakeEventBusPublisher struct {
	mu    sync.Mutex
	calls []eventBusCall
	err   error
}

type eventBusCall struct {
	subject string
	payload []byte
}

func (p *fakeEventBusPublisher) Publish(_ context.Context, subject string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		err := p.err
		p.err = nil
		return err
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	p.calls = append(p.calls, eventBusCall{subject: subject, payload: cp})
	return nil
}

func (p *fakeEventBusPublisher) snapshot() []eventBusCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]eventBusCall, len(p.calls))
	copy(out, p.calls)
	return out
}

// newProgressTrackerT bundles miniredis + go-redis + a recording
// publisher for tests of the production ProgressTracker. The redis
// client is returned alongside so tests can inspect the hash directly.
func newProgressTrackerT(t *testing.T) (*ProgressTracker, redis.UniversalClient, *fakeEventBusPublisher) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	pub := &fakeEventBusPublisher{}
	clock := func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) }
	return NewProgressTracker(rdb, pub, nil, clock), rdb, pub
}

func TestProgressTracker_InitWritesQueuedWhenTotalZero(t *testing.T) {
	t.Parallel()

	pt, rdb, _ := newProgressTrackerT(t)
	require.NoError(t, pt.Init(context.Background(), "job-1", uuid.New(), 0))

	values, err := rdb.HGetAll(context.Background(), importStatusKey("job-1")).Result()
	require.NoError(t, err)
	require.Equal(t, "queued", values[fieldStatus])
	require.Equal(t, "0", values[fieldTotal])
}

func TestProgressTracker_InitWritesRunningWhenTotalKnown(t *testing.T) {
	t.Parallel()

	pt, rdb, _ := newProgressTrackerT(t)
	require.NoError(t, pt.Init(context.Background(), "job-1", uuid.New(), 5))

	values, err := rdb.HGetAll(context.Background(), importStatusKey("job-1")).Result()
	require.NoError(t, err)
	require.Equal(t, "running", values[fieldStatus])
	require.Equal(t, "5", values[fieldTotal])
}

func TestProgressTracker_UpdatePublishesEvent(t *testing.T) {
	t.Parallel()

	pt, _, pub := newProgressTrackerT(t)
	tenantID := uuid.New()
	require.NoError(t, pt.Init(context.Background(), "j", tenantID, 10))
	require.NoError(t, pt.Update(context.Background(), "j", tenantID, 5, 3, 2))

	calls := pub.snapshot()
	// Update publishes one event; Init does not.
	require.Len(t, calls, 1)
	require.Equal(t, crmapi.SubjectImportProgressFor(tenantID), calls[0].subject)
}

func TestProgressTracker_FinishWritesSucceededAndPublishes(t *testing.T) {
	t.Parallel()

	pt, rdb, pub := newProgressTrackerT(t)
	tenantID := uuid.New()
	require.NoError(t, pt.Init(context.Background(), "j", tenantID, 10))
	require.NoError(t, pt.Finish(context.Background(), "j", tenantID, 10, 9, 1))

	values, err := rdb.HGetAll(context.Background(), importStatusKey("j")).Result()
	require.NoError(t, err)
	require.Equal(t, "succeeded", values[fieldStatus])
	require.Equal(t, "10", values[fieldTotal])
	require.Equal(t, "9", values[fieldInserted])
	require.Equal(t, "1", values[fieldSkipped])

	calls := pub.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, crmapi.SubjectImportFinishedFor(tenantID), calls[0].subject)
}

func TestProgressTracker_FailWritesFailedAndPublishes(t *testing.T) {
	t.Parallel()

	pt, rdb, pub := newProgressTrackerT(t)
	tenantID := uuid.New()
	require.NoError(t, pt.Init(context.Background(), "j", tenantID, 0))
	require.NoError(t, pt.Fail(context.Background(), "j", tenantID, "boom"))

	values, err := rdb.HGetAll(context.Background(), importStatusKey("j")).Result()
	require.NoError(t, err)
	require.Equal(t, "failed", values[fieldStatus])
	require.Equal(t, "boom", values[fieldError])

	calls := pub.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, crmapi.SubjectImportFailedFor(tenantID), calls[0].subject)
}

func TestProgressTracker_StatusReturnsErrImportNotFoundForMissing(t *testing.T) {
	t.Parallel()

	pt, _, _ := newProgressTrackerT(t)
	_, err := pt.Status(context.Background(), "missing")
	require.ErrorIs(t, err, crmapi.ErrImportNotFound)
}

func TestProgressTracker_StatusRoundTripsAllFields(t *testing.T) {
	t.Parallel()

	pt, _, _ := newProgressTrackerT(t)
	tenantID := uuid.New()
	require.NoError(t, pt.Init(context.Background(), "j", tenantID, 10))
	require.NoError(t, pt.Update(context.Background(), "j", tenantID, 6, 5, 1))
	require.NoError(t, pt.Finish(context.Background(), "j", tenantID, 10, 9, 1))

	st, err := pt.Status(context.Background(), "j")
	require.NoError(t, err)
	require.Equal(t, "succeeded", st.State)
	require.Equal(t, 10, st.Total)
	require.Equal(t, 9, st.Inserted)
	require.Equal(t, 1, st.Skipped)
	require.NotNil(t, st.FinishedAt)
}

func TestProgressTracker_PublisherFailureDoesNotBubbleUp(t *testing.T) {
	t.Parallel()

	pt, _, pub := newProgressTrackerT(t)
	pub.err = errors.New("nats unavailable")
	tenantID := uuid.New()
	require.NoError(t, pt.Init(context.Background(), "j", tenantID, 1))
	// Update is the first method that calls publish; it should not
	// surface the publisher's error.
	require.NoError(t, pt.Update(context.Background(), "j", tenantID, 1, 1, 0))
}

func TestProgressTracker_NilRedisIsNoOp(t *testing.T) {
	t.Parallel()

	pt := NewProgressTracker(nil, nil, nil, nil)
	require.NoError(t, pt.Init(context.Background(), "x", uuid.New(), 0))
	require.NoError(t, pt.Update(context.Background(), "x", uuid.New(), 0, 0, 0))
	require.NoError(t, pt.Finish(context.Background(), "x", uuid.New(), 0, 0, 0))
	require.NoError(t, pt.Fail(context.Background(), "x", uuid.New(), "msg"))
	_, err := pt.Status(context.Background(), "x")
	require.ErrorIs(t, err, crmapi.ErrImportNotFound)
}

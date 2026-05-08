package retry_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/retry"
)

// fakeLeader is an in-memory Leader for unit tests. Acquire returns
// the configured (ok, err); Release flips IsLeading off.
type fakeLeader struct {
	mu       sync.Mutex
	leading  bool
	acquired int
	released int
	nextOK   bool
	nextErr  error
}

func (f *fakeLeader) Acquire(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired++
	if f.nextErr != nil {
		return false, f.nextErr
	}
	f.leading = f.nextOK
	return f.nextOK, nil
}

func (f *fakeLeader) Release(_ context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
	f.leading = false
}

func (f *fakeLeader) IsLeading() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leading
}

// fakeReader is an in-memory RespondentReader. The sweep returns the
// rows queued via setRows; MarkExhausted / MarkScheduled record their
// arguments so tests can assert on them.
type fakeReader struct {
	mu          sync.Mutex
	rows        []retry.MatureRetryRow
	listErr     error
	exhausted   []uuid.UUID
	scheduled   []uuid.UUID
	exhaustErr  error
	scheduleErr error
}

func (r *fakeReader) ListMatureRetries(_ context.Context, _ int) ([]retry.MatureRetryRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]retry.MatureRetryRow, len(r.rows))
	copy(out, r.rows)
	// Drain so the next sweep is empty unless the test re-loads.
	r.rows = nil
	return out, nil
}

func (r *fakeReader) MarkExhausted(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exhaustErr != nil {
		return r.exhaustErr
	}
	r.exhausted = append(r.exhausted, id)
	return nil
}

func (r *fakeReader) MarkScheduled(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scheduleErr != nil {
		return r.scheduleErr
	}
	r.scheduled = append(r.scheduled, id)
	return nil
}

func (r *fakeReader) setRows(rows []retry.MatureRetryRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = rows
}

// fakeDecryptor returns the ciphertext bytes verbatim — tests pass
// plain ASCII so the orchestrator's "phone" is just the bytes.
type fakeDecryptor struct {
	err error
}

func (d fakeDecryptor) Decrypt(_ context.Context, _ uuid.UUID, ciphertext []byte) ([]byte, error) {
	if d.err != nil {
		return nil, d.err
	}
	return ciphertext, nil
}

// fakeQueue records every EnqueueRespondent call. Other CallQueue
// methods are stubbed since the orchestrator only calls Enqueue.
type fakeQueue struct {
	mu         sync.Mutex
	enqueued   []api.EnqueueRequest
	enqueueErr error
	enqueueDup bool // when true, returns ok=false (already queued)
}

func (q *fakeQueue) EnqueueRespondent(_ context.Context, req api.EnqueueRequest) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.enqueueErr != nil {
		return false, q.enqueueErr
	}
	q.enqueued = append(q.enqueued, req)
	if q.enqueueDup {
		return false, nil
	}
	return true, nil
}

func (q *fakeQueue) PickNext(_ context.Context, _, _ uuid.UUID) (api.QueueItem, error) {
	return api.QueueItem{}, api.ErrQueueEmpty
}

func (q *fakeQueue) Requeue(_ context.Context, _ api.QueueItem, _ time.Duration) error {
	return nil
}

func (q *fakeQueue) Size(_ context.Context, _, _ uuid.UUID) (int64, error) {
	return 0, nil
}

func (q *fakeQueue) Remove(_ context.Context, _, _, _ uuid.UUID) error {
	return nil
}

// newOrchestrator builds an Orchestrator wired against the supplied
// fakes. The interval is set to a short duration so Run cycles quickly
// in tests; tests cancel ctx before the second tick fires.
func newOrchestrator(
	t *testing.T,
	leader retry.Leader,
	reader retry.RespondentReader,
	dec retry.Decryptor,
	queue api.CallQueue,
) (*retry.Orchestrator, *retry.Metrics) {
	t.Helper()
	reg := prometheus.NewRegistry()
	metrics := retry.RegisterMetrics(reg)
	o, err := retry.New(retry.Config{
		Leader:      leader,
		Reader:      reader,
		Decryptor:   dec,
		Queue:       queue,
		Interval:    50 * time.Millisecond,
		BatchLimit:  10,
		MaxAttempts: 3,
		Logger:      zaptest.NewLogger(t),
		Metrics:     metrics,
	})
	require.NoError(t, err)
	return o, metrics
}

// runOrchestratorOnce runs the orchestrator and returns when the
// fakeLeader has seen at least `nAcquires` Acquire calls. Then cancels
// ctx and waits for Run to return. Helper for sweep-shape tests.
func runOrchestratorOnce(t *testing.T, o *retry.Orchestrator, leader *fakeLeader, nAcquires int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()

	// Wait until Run has performed at least nAcquires acquire attempts.
	deadline := time.Now().Add(5 * time.Second)
	for {
		leader.mu.Lock()
		got := leader.acquired
		leader.mu.Unlock()
		if got >= nAcquires {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("timeout waiting for %d Acquire calls; got %d", nAcquires, got)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// counterValue extracts the float value of the Sweeps CounterVec for
// the supplied label. Returns 0 when the series has not been observed.
func counterValue(t *testing.T, m *retry.Metrics, label string) float64 {
	t.Helper()
	c, err := m.Sweeps.GetMetricWithLabelValues(label)
	require.NoError(t, err)
	return testutil.ToFloat64(c)
}

// TestNew_RejectsMissingDependencies surfaces a wiring bug at boot
// rather than at first sweep tick.
func TestNew_RejectsMissingDependencies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  retry.Config
	}{
		{name: "missing leader", cfg: retry.Config{
			Reader:    &fakeReader{},
			Decryptor: fakeDecryptor{},
			Queue:     &fakeQueue{},
		}},
		{name: "missing reader", cfg: retry.Config{
			Leader:    &fakeLeader{},
			Decryptor: fakeDecryptor{},
			Queue:     &fakeQueue{},
		}},
		{name: "missing decryptor", cfg: retry.Config{
			Leader: &fakeLeader{},
			Reader: &fakeReader{},
			Queue:  &fakeQueue{},
		}},
		{name: "missing queue", cfg: retry.Config{
			Leader:    &fakeLeader{},
			Reader:    &fakeReader{},
			Decryptor: fakeDecryptor{},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, err := retry.New(tc.cfg)
			require.Error(t, err)
			require.Nil(t, o)
		})
	}
}

// TestNew_AppliesDefaults constructs an Orchestrator with only the
// required fields and confirms it boots (zero-valued Interval / Batch /
// MaxAttempts fall back to the sane defaults). The String() helper
// surfaces the effective values.
func TestNew_AppliesDefaults(t *testing.T) {
	t.Parallel()
	o, err := retry.New(retry.Config{
		Leader:    &fakeLeader{},
		Reader:    &fakeReader{},
		Decryptor: fakeDecryptor{},
		Queue:     &fakeQueue{},
	})
	require.NoError(t, err)
	require.Contains(t, o.String(), "interval=30s")
	require.Contains(t, o.String(), "batch=100")
	require.Contains(t, o.String(), "max_attempts=3")
}

// TestRun_LeaderSweepEnqueues — happy-path: this instance leads, two
// rows are mature, both enqueue and are marked scheduled.
func TestRun_LeaderSweepEnqueues(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	project := uuid.New()
	respA := uuid.New()
	respB := uuid.New()

	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{}
	reader.setRows([]retry.MatureRetryRow{
		{
			ID:              respA,
			TenantID:        tenant,
			ProjectID:       project,
			PhoneCiphertext: []byte("+79991234567"),
			Region:          "RU-MOW",
			Attempts:        1,
		},
		{
			ID:              respB,
			TenantID:        tenant,
			ProjectID:       project,
			PhoneCiphertext: []byte("+79991234568"),
			Region:          "RU-SPE",
			Attempts:        2, // attempts < maxAttempts(3), still retries
		},
	})
	queue := &fakeQueue{}
	o, metrics := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	// Both rows enqueued.
	queue.mu.Lock()
	defer queue.mu.Unlock()
	require.Len(t, queue.enqueued, 2)

	// Phone passed through verbatim from fakeDecryptor.
	require.Equal(t, "+79991234567", queue.enqueued[0].Phone)
	require.Equal(t, "+79991234568", queue.enqueued[1].Phone)
	// AttemptN is row.Attempts + 1.
	require.Equal(t, uint8(2), queue.enqueued[0].AttemptN)
	require.Equal(t, uint8(3), queue.enqueued[1].AttemptN)
	// Priority = min(1+attempts, 9).
	require.Equal(t, uint8(2), queue.enqueued[0].Priority)
	require.Equal(t, uint8(3), queue.enqueued[1].Priority)

	// Both rows marked scheduled in DB.
	reader.mu.Lock()
	require.ElementsMatch(t, []uuid.UUID{respA, respB}, reader.scheduled)
	require.Empty(t, reader.exhausted)
	reader.mu.Unlock()

	require.InDelta(t, 2.0, counterValue(t, metrics, "enqueued"), 0)
}

// TestRun_LeaderSweepExhausts — a row at the cap goes straight to
// MarkExhausted; never touches the queue.
func TestRun_LeaderSweepExhausts(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	project := uuid.New()
	respA := uuid.New()

	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{}
	reader.setRows([]retry.MatureRetryRow{
		{
			ID:              respA,
			TenantID:        tenant,
			ProjectID:       project,
			PhoneCiphertext: []byte("+79991234567"),
			Region:          "RU-MOW",
			Attempts:        3, // == maxAttempts → exhausted
		},
	})
	queue := &fakeQueue{}
	o, metrics := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	queue.mu.Lock()
	require.Empty(t, queue.enqueued, "exhausted rows must not enqueue")
	queue.mu.Unlock()

	reader.mu.Lock()
	require.Equal(t, []uuid.UUID{respA}, reader.exhausted)
	require.Empty(t, reader.scheduled)
	reader.mu.Unlock()

	require.InDelta(t, 1.0, counterValue(t, metrics, "exhausted"), 0)
}

// TestRun_NotLeaderSkipsSweep — when Acquire returns ok=false the
// orchestrator does not touch reader / queue. leader_active=0 is set.
func TestRun_NotLeaderSkipsSweep(t *testing.T) {
	t.Parallel()
	leader := &fakeLeader{nextOK: false}
	reader := &fakeReader{}
	reader.setRows([]retry.MatureRetryRow{
		{ID: uuid.New(), TenantID: uuid.New(), ProjectID: uuid.New()},
	})
	queue := &fakeQueue{}
	o, metrics := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	queue.mu.Lock()
	require.Empty(t, queue.enqueued)
	queue.mu.Unlock()

	reader.mu.Lock()
	require.Empty(t, reader.scheduled)
	require.Empty(t, reader.exhausted)
	reader.mu.Unlock()

	// leader_active=0 throughout: gauge value is 0.
	require.InDelta(t, 0.0, testutil.ToFloat64(metrics.LeaderActive), 0)
}

// TestRun_AcquireErrorContinues — a transient leader error logs and
// skips the sweep without crashing the loop.
func TestRun_AcquireErrorContinues(t *testing.T) {
	t.Parallel()
	leader := &fakeLeader{nextErr: errors.New("transient PG outage")}
	reader := &fakeReader{}
	queue := &fakeQueue{}
	o, _ := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	// Two acquire attempts; the loop must survive both.
	runOrchestratorOnce(t, o, leader, 2)
}

// TestRun_DecryptErrorSkipsRow — a per-row decrypt failure is logged
// and the row is bucketed under "skip" but the rest of the sweep
// continues.
func TestRun_DecryptErrorSkipsRow(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	project := uuid.New()
	respA := uuid.New()
	respB := uuid.New()

	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{}
	reader.setRows([]retry.MatureRetryRow{
		{ID: respA, TenantID: tenant, ProjectID: project, PhoneCiphertext: []byte("a"), Attempts: 0},
		{ID: respB, TenantID: tenant, ProjectID: project, PhoneCiphertext: []byte("b"), Attempts: 0},
	})
	queue := &fakeQueue{}

	dec := &errOnFirstDecryptor{}
	o, metrics := newOrchestrator(t, leader, reader, dec, queue)

	runOrchestratorOnce(t, o, leader, 1)

	// Second row enqueued; first counted as skip.
	queue.mu.Lock()
	require.Len(t, queue.enqueued, 1)
	require.Equal(t, respB, queue.enqueued[0].RespondentID)
	queue.mu.Unlock()

	require.InDelta(t, 1.0, counterValue(t, metrics, "skip"), 0)
	require.InDelta(t, 1.0, counterValue(t, metrics, "enqueued"), 0)
}

// errOnFirstDecryptor fails the FIRST decrypt call and succeeds for
// every subsequent one. Used to test per-row error isolation.
type errOnFirstDecryptor struct {
	mu    sync.Mutex
	count int
}

func (d *errOnFirstDecryptor) Decrypt(_ context.Context, _ uuid.UUID, ciphertext []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.count++
	if d.count == 1 {
		return nil, errors.New("decrypt boom")
	}
	return ciphertext, nil
}

// TestRun_QueueErrorSkipsRow — queue.EnqueueRespondent failure logs
// and skips; row not marked scheduled.
func TestRun_QueueErrorSkipsRow(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	project := uuid.New()
	respA := uuid.New()

	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{}
	reader.setRows([]retry.MatureRetryRow{
		{ID: respA, TenantID: tenant, ProjectID: project, PhoneCiphertext: []byte("+1"), Attempts: 0},
	})
	queue := &fakeQueue{enqueueErr: errors.New("redis down")}
	o, metrics := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	reader.mu.Lock()
	require.Empty(t, reader.scheduled, "row must not be marked scheduled if enqueue fails")
	reader.mu.Unlock()

	require.InDelta(t, 1.0, counterValue(t, metrics, "skip"), 0)
}

// TestRun_QueueDuplicateStillMarksScheduled — enqueue returns
// ok=false (already in queue). The row still gets MarkScheduled so the
// sweep doesn't keep re-picking it.
func TestRun_QueueDuplicateStillMarksScheduled(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	project := uuid.New()
	respA := uuid.New()

	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{}
	reader.setRows([]retry.MatureRetryRow{
		{ID: respA, TenantID: tenant, ProjectID: project, PhoneCiphertext: []byte("+1"), Attempts: 0},
	})
	queue := &fakeQueue{enqueueDup: true}
	o, metrics := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	reader.mu.Lock()
	require.Equal(t, []uuid.UUID{respA}, reader.scheduled, "duplicate path still marks scheduled")
	reader.mu.Unlock()

	require.InDelta(t, 1.0, counterValue(t, metrics, "enqueued"), 0)
}

// TestRun_ListErrorContinues — a sweep where ListMatureRetries fails
// logs the error and continues to the next tick (no crash, no leak).
func TestRun_ListErrorContinues(t *testing.T) {
	t.Parallel()
	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{listErr: errors.New("pg down")}
	queue := &fakeQueue{}
	o, _ := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	queue.mu.Lock()
	require.Empty(t, queue.enqueued)
	queue.mu.Unlock()
}

// TestRun_EmptyBatchIsNoOp — when the reader returns no rows the sweep
// is a quiet no-op (no metric increments, no leader change).
func TestRun_EmptyBatchIsNoOp(t *testing.T) {
	t.Parallel()
	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{} // no rows
	queue := &fakeQueue{}
	o, metrics := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	require.InDelta(t, 0.0, counterValue(t, metrics, "enqueued"), 0)
	require.InDelta(t, 0.0, counterValue(t, metrics, "exhausted"), 0)
	require.InDelta(t, 0.0, counterValue(t, metrics, "skip"), 0)
}

// TestRun_ReleasesLeaderOnCancel — a leading instance Releases when
// the context is cancelled.
func TestRun_ReleasesLeaderOnCancel(t *testing.T) {
	t.Parallel()
	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{}
	queue := &fakeQueue{}
	o, _ := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	leader.mu.Lock()
	defer leader.mu.Unlock()
	require.GreaterOrEqual(t, leader.released, 1, "Run must Release on ctx cancel")
}

// TestRun_MarkExhaustedErrorIsBucketedAsSkip — the row would be
// exhausted but the DB write fails; the failure is bucketed under
// "skip" so dashboards aren't lied to.
func TestRun_MarkExhaustedErrorIsBucketedAsSkip(t *testing.T) {
	t.Parallel()
	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{exhaustErr: errors.New("pg blip")}
	reader.setRows([]retry.MatureRetryRow{
		{ID: uuid.New(), TenantID: uuid.New(), ProjectID: uuid.New(),
			PhoneCiphertext: []byte("+1"), Attempts: 3},
	})
	queue := &fakeQueue{}
	o, metrics := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	require.InDelta(t, 1.0, counterValue(t, metrics, "skip"), 0)
	require.InDelta(t, 0.0, counterValue(t, metrics, "exhausted"), 0)
}

// TestRun_MarkScheduledErrorIsBucketedAsSkip — enqueue succeeds but
// MarkScheduled fails; metric is "skip".
func TestRun_MarkScheduledErrorIsBucketedAsSkip(t *testing.T) {
	t.Parallel()
	leader := &fakeLeader{nextOK: true}
	reader := &fakeReader{scheduleErr: errors.New("pg blip")}
	reader.setRows([]retry.MatureRetryRow{
		{ID: uuid.New(), TenantID: uuid.New(), ProjectID: uuid.New(),
			PhoneCiphertext: []byte("+1"), Attempts: 0},
	})
	queue := &fakeQueue{}
	o, metrics := newOrchestrator(t, leader, reader, fakeDecryptor{}, queue)

	runOrchestratorOnce(t, o, leader, 1)

	require.InDelta(t, 1.0, counterValue(t, metrics, "skip"), 0)
}

// TestRegisterMetrics_PanicsOnNil — wiring bug surfaces at boot.
func TestRegisterMetrics_PanicsOnNil(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		retry.RegisterMetrics(nil)
	})
}

// TestNewPgLeader_RejectsNilPool surfaces a wiring bug.
func TestNewPgLeader_RejectsNilPool(t *testing.T) {
	t.Parallel()
	_, err := retry.NewPgLeader(nil, 0, nil)
	require.Error(t, err)
}

// TestPgLeader_DefaultLockKey — supplying key=0 falls back to the
// default FNV hash; the default is non-zero (otherwise the constant
// would wrap to the same value as an explicit 0).
func TestPgLeader_DefaultLockKey(t *testing.T) {
	t.Parallel()
	require.NotZero(t, retry.DefaultLockKey,
		"DefaultLockKey should be a stable, non-zero FNV hash")
}

// TestNewPgReader_RejectsNilPool surfaces a wiring bug.
func TestNewPgReader_RejectsNilPool(t *testing.T) {
	t.Parallel()
	_, err := retry.NewPgReader(nil)
	require.Error(t, err)
}

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakePurgeRunner implements purgeBypassRunner for tests. Mirrors
// fakeRespondentTxRunner but exposes only BypassRLS — the worker
// never opens per-tenant transactions.
type fakePurgeRunner struct {
	mu          sync.Mutex
	bypassCount int
}

func (f *fakePurgeRunner) BypassRLS(_ context.Context, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	f.bypassCount++
	f.mu.Unlock()
	return fn(postgres.Tx{})
}

// fakePurgeStore is a small api.RespondentStorePort fake focused on
// PurgeOlderThan + the methods the worker doesn't touch (which are
// satisfied with no-op error returns for compile-time conformance).
type fakePurgeStore struct {
	mu sync.Mutex

	purgeFn      func(cutoff time.Time, limit int) ([]uuid.UUID, error)
	purgeCalls   []time.Time
	purgeLimits  []int
	purgeErrFlag bool
}

func (s *fakePurgeStore) Insert(_ context.Context, _ postgres.Tx, _ crmapi.Respondent) (crmapi.Respondent, error) {
	return crmapi.Respondent{}, errors.New("unused")
}
func (s *fakePurgeStore) GetByID(_ context.Context, _ postgres.Tx, _ uuid.UUID) (crmapi.Respondent, error) {
	return crmapi.Respondent{}, errors.New("unused")
}
func (s *fakePurgeStore) GetByHash(_ context.Context, _ postgres.Tx, _, _ uuid.UUID, _ []byte) (crmapi.Respondent, error) {
	return crmapi.Respondent{}, errors.New("unused")
}
func (s *fakePurgeStore) IsBlockedDNC(_ context.Context, _ postgres.Tx, _, _ uuid.UUID, _ []byte) (bool, error) {
	return false, errors.New("unused")
}
func (s *fakePurgeStore) InsertBatch(_ context.Context, _ postgres.Tx, _ []crmapi.Respondent) (int, error) {
	return 0, errors.New("unused")
}
func (s *fakePurgeStore) ExistingHashes(_ context.Context, _ postgres.Tx, _, _ uuid.UUID, _ [][]byte) ([][]byte, error) {
	return nil, errors.New("unused")
}
func (s *fakePurgeStore) SoftDelete(_ context.Context, _ postgres.Tx, _ uuid.UUID, _ string, _ time.Time) error {
	return errors.New("unused")
}
func (s *fakePurgeStore) PurgeOlderThan(_ context.Context, _ postgres.Tx, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeCalls = append(s.purgeCalls, cutoff)
	s.purgeLimits = append(s.purgeLimits, limit)
	if s.purgeErrFlag {
		s.purgeErrFlag = false
		return nil, errors.New("db down")
	}
	if s.purgeFn != nil {
		return s.purgeFn(cutoff, limit)
	}
	return nil, nil
}
func (s *fakePurgeStore) Search(_ context.Context, _ postgres.Tx, _ crmapi.SearchRespondentsFilter) ([]crmapi.Respondent, int64, error) {
	return nil, 0, errors.New("unused")
}

// fakePurgeAudit captures the audit rows the worker writes. Mirrors
// fakeRespondentAudit; defined locally so the test binary stays
// independent.
type fakePurgeAudit struct {
	mu     sync.Mutex
	events []auditapi.Event
	err    error
}

func (a *fakePurgeAudit) Write(_ context.Context, ev auditapi.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		err := a.err
		a.err = nil
		return err
	}
	a.events = append(a.events, ev)
	return nil
}

func (a *fakePurgeAudit) snapshot() []auditapi.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]auditapi.Event, len(a.events))
	copy(out, a.events)
	return out
}

func newPurgeWorker(t *testing.T) (
	*PurgeWorker,
	*fakePurgeRunner,
	*fakePurgeStore,
	*fakePurgeAudit,
) {
	t.Helper()
	tx := &fakePurgeRunner{}
	store := &fakePurgeStore{}
	audit := &fakePurgeAudit{}
	clock := func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) }
	w := NewPurgeWorker(tx, store, audit, 30*24*time.Hour, 1000, clock)
	return w, tx, store, audit
}

func TestPurgeWorker_NoRowsToPurge(t *testing.T) {
	t.Parallel()

	w, tx, store, audit := newPurgeWorker(t)
	store.purgeFn = func(_ time.Time, _ int) ([]uuid.UUID, error) {
		return nil, nil
	}

	require.NoError(t, w.Run(context.Background()))
	require.Equal(t, 1, tx.bypassCount)
	require.Empty(t, audit.snapshot(), "no audit rows when nothing was purged")
}

func TestPurgeWorker_PurgesAndAudits(t *testing.T) {
	t.Parallel()

	w, _, store, audit := newPurgeWorker(t)
	id1 := uuid.New()
	id2 := uuid.New()
	store.purgeFn = func(_ time.Time, _ int) ([]uuid.UUID, error) {
		return []uuid.UUID{id1, id2}, nil
	}

	require.NoError(t, w.Run(context.Background()))
	events := audit.snapshot()
	require.Len(t, events, 2)
	require.Equal(t, "crm.respondent.purged", events[0].Action)
	require.Equal(t, "respondent:"+id1.String(), events[0].Target)
	require.Equal(t, auditapi.ActorSystem, events[0].ActorKind)
	require.Equal(t, "crm.respondent.purged", events[1].Action)
	require.Equal(t, "respondent:"+id2.String(), events[1].Target)
}

func TestPurgeWorker_ComputesCutoffFromClock(t *testing.T) {
	t.Parallel()

	w, _, store, _ := newPurgeWorker(t)

	require.NoError(t, w.Run(context.Background()))
	require.Len(t, store.purgeCalls, 1)
	expected := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC).Add(-30 * 24 * time.Hour)
	require.True(t, store.purgeCalls[0].Equal(expected),
		"cutoff must be now - grace; got %v want %v", store.purgeCalls[0], expected)
}

func TestPurgeWorker_PassesBatchLimit(t *testing.T) {
	t.Parallel()

	w, _, store, _ := newPurgeWorker(t)
	require.NoError(t, w.Run(context.Background()))
	require.Equal(t, []int{1000}, store.purgeLimits)
}

func TestPurgeWorker_StoreFailureBubblesUp(t *testing.T) {
	t.Parallel()

	w, _, store, audit := newPurgeWorker(t)
	store.purgeErrFlag = true

	err := w.Run(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "db down")
	require.Empty(t, audit.snapshot(), "no audit rows when the purge query failed")
}

func TestPurgeWorker_AuditFailureNonFatal(t *testing.T) {
	t.Parallel()

	w, _, store, audit := newPurgeWorker(t)
	store.purgeFn = func(_ time.Time, _ int) ([]uuid.UUID, error) {
		return []uuid.UUID{uuid.New()}, nil
	}
	audit.err = errors.New("audit down")

	// Audit failure must NOT fail the task — the row is already
	// hard-deleted and a retry would emit duplicate audit rows.
	require.NoError(t, w.Run(context.Background()))
}

func TestPurgeWorker_HandlePurgeTaskDelegatesToRun(t *testing.T) {
	t.Parallel()

	w, _, store, audit := newPurgeWorker(t)
	id := uuid.New()
	store.purgeFn = func(_ time.Time, _ int) ([]uuid.UUID, error) {
		return []uuid.UUID{id}, nil
	}

	task := asynq.NewTask(crmapi.TaskRespondentsPurge, nil)
	require.NoError(t, w.HandlePurgeTask(context.Background(), task))
	require.Len(t, audit.snapshot(), 1)
}

// TestPurgeWorker_BatchOfThousand exercises the batch boundary — the
// worker must process every row in the slice the store returns.
func TestPurgeWorker_BatchOfThousand(t *testing.T) {
	t.Parallel()

	w, _, store, audit := newPurgeWorker(t)
	ids := make([]uuid.UUID, 1000)
	for i := range ids {
		ids[i] = uuid.New()
	}
	store.purgeFn = func(_ time.Time, _ int) ([]uuid.UUID, error) {
		return ids, nil
	}

	require.NoError(t, w.Run(context.Background()))
	require.Len(t, audit.snapshot(), 1000)
}

func TestNewPurgeWorker_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	tx := &fakePurgeRunner{}
	store := &fakePurgeStore{}
	audit := &fakePurgeAudit{}

	cases := []struct {
		name string
		fn   func()
	}{
		{"nil pool", func() { _ = NewPurgeWorker(nil, store, audit, 0, 0, nil) }},
		{"nil store", func() { _ = NewPurgeWorker(tx, nil, audit, 0, 0, nil) }},
		{"nil audit", func() { _ = NewPurgeWorker(tx, store, nil, 0, 0, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Panics(t, tc.fn)
		})
	}
}

func TestNewPurgeWorker_DefaultsAreSane(t *testing.T) {
	t.Parallel()

	w := NewPurgeWorker(&fakePurgeRunner{}, &fakePurgeStore{}, &fakePurgeAudit{}, 0, 0, nil)
	require.Equal(t, deletionGracePeriod, w.grace)
	require.Equal(t, defaultPurgeBatch, w.batch)
	require.NotNil(t, w.clock)
}

// TestPurgeWorker_WithLoggerNilFallsBackToNop ensures the optional
// logger setter never leaves the worker with a nil logger.
func TestPurgeWorker_WithLoggerNilFallsBackToNop(t *testing.T) {
	t.Parallel()

	w := NewPurgeWorker(&fakePurgeRunner{}, &fakePurgeStore{}, &fakePurgeAudit{}, 0, 0, nil)
	w = w.WithLogger(nil)
	require.NotNil(t, w.logger)
}

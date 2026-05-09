//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap/zaptest"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/internal/recording/worker"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fakes
// ─────────────────────────────────────────────────────────────────────────────

// fakeLeader is an always-leading Leader. The unit-style tests in this
// file drive SweepOnce directly so the leader is irrelevant — we still
// pass one because RetentionConfig validates non-nil deps.
type fakeLeader struct {
	mu       sync.Mutex
	acquired int
	released int
}

func (f *fakeLeader) Acquire(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired++
	return true, nil
}

func (f *fakeLeader) Release(_ context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
}

func (f *fakeLeader) Key() int64 { return worker.RetentionLockKey }

// deleteCall records the (bucket, key) pair that fakeObjectStore.Delete
// was invoked with. Tests assert on the slice for ordering + count.
type deleteCall struct {
	bucket string
	key    string
}

// fakeObjectStore satisfies storage.ObjectStore. Delete records the call
// + returns nextErr; Get is unused by the worker so it returns a stub
// error. nextErr is staged BEFORE SweepOnce so the test simulates a
// transient outage or an already-gone object.
type fakeObjectStore struct {
	mu      sync.Mutex
	deletes []deleteCall
	nextErr error
}

// Compile-time check.
var _ storage.ObjectStore = (*fakeObjectStore)(nil)

func (s *fakeObjectStore) Get(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, errors.New("fakeObjectStore.Get: not used by worker tests")
}

func (s *fakeObjectStore) Delete(_ context.Context, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, deleteCall{bucket: bucket, key: key})
	return s.nextErr
}

func (s *fakeObjectStore) deleteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deletes)
}

func (s *fakeObjectStore) firstDelete() deleteCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deletes) == 0 {
		return deleteCall{}
	}
	return s.deletes[0]
}

// fakeOutboxWriter records every Append call. The writer accepts the
// event verbatim; tests assert on the recorded subjects + payloads.
type fakeOutboxWriter struct {
	mu     sync.Mutex
	events []outbox.Event
	err    error
}

func (w *fakeOutboxWriter) Append(_ context.Context, _ postgres.Tx, ev outbox.Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.events = append(w.events, ev)
	return nil
}

func (w *fakeOutboxWriter) snapshot() []outbox.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]outbox.Event, len(w.events))
	copy(out, w.events)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Test container + seeding helpers (cloned from store_pg_test.go pattern)
// ─────────────────────────────────────────────────────────────────────────────

func startPGContainer(t *testing.T) *postgres.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("sociopulse_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	migrationsURL := repoMigrationsURL(t)
	mig, err := migrate.New(migrationsURL, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = mig.Close() })
	require.NoError(t, mig.Up())

	pool, err := postgres.Open(ctx, postgres.Config{
		DSN:            dsn,
		MaxConns:       8,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func repoMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err, "migrations dir not found at %s", abs)
	return "file://" + abs
}

func seedTenant(t *testing.T, pool *postgres.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			 VALUES ($1, $2, $3, 'active', 'kms-test', '\x00')`,
			id,
			"org-"+id.String(),
			"tenant-"+id.String(),
		)
		return err
	}))
	return id
}

func seedCallInTenant(t *testing.T, pool *postgres.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	projectID := uuid.Must(uuid.NewV7())
	callID := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status)
			 VALUES ($1, $2, $3, 'Test Project', 'active')`,
			projectID, tenantID, "proj-"+projectID.String(),
		); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(),
			`INSERT INTO calls (id, tenant_id, project_id, started_at, status)
			 VALUES ($1, $2, $3, now(), 'success')`,
			callID, tenantID, projectID,
		)
		return err
	}))
	return callID
}

// newRow builds a fully populated RecordingRow for tests.
func newRow(t *testing.T, tenantID, callID uuid.UUID) store.RecordingRow {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	deleteAt := now.Add(730 * 24 * time.Hour)
	return store.RecordingRow{
		ID:             uuid.Must(uuid.NewV7()),
		CallID:         callID,
		TenantID:       tenantID,
		S3Bucket:       "rec-bucket-1",
		AudioObjectKey: "recordings/x/x/x/x.opus.enc",
		KMSKeyID:       "kms-key-1",
		EncryptedDEK:   []byte("encrypted-dek-stub-32bytes-xxxxx"),
		BytesSize:      1234567,
		DurationMS:     12345,
		SHA256Hex:      "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100",
		Codec:          "opus",
		SampleRate:     48000,
		Status:         "stored",
		CommittedAt:    now,
		DeleteAt:       &deleteAt,
		ColdAt:         now.Add(365 * 24 * time.Hour),
		RecordedAt:     now.Add(-1 * time.Hour),
		IngestAgentID:  "agent-test",
	}
}

// auditCount returns the number of audit_log rows for (tenantID, action).
//
// Uses WithTenant because the audit_log table is RLS-protected and the
// app_user has CRUD via the tenant predicate; tenancy_admin only has
// DML on tenants + tenant_settings (see migrations/000001_init.up.sql
// "grants" block).
func auditCount(t *testing.T, pool *postgres.Pool, tenantID uuid.UUID, action string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(t.Context(),
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`,
			tenantID, action,
		).Scan(&n)
	}))
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// Fixture builder
// ─────────────────────────────────────────────────────────────────────────────

type fixture struct {
	pool   *postgres.Pool
	store  *store.PostgresStore
	leader *fakeLeader
	objs   *fakeObjectStore
	out    *fakeOutboxWriter
	mtr    *metrics.RecordingMetrics
	pass   *worker.RetentionPass
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)
	leader := &fakeLeader{}
	objs := &fakeObjectStore{}
	out := &fakeOutboxWriter{}
	reg := prometheus.NewRegistry()
	mtr, err := metrics.RegisterRecordingMetrics(reg)
	require.NoError(t, err)

	pass, err := worker.NewRetentionPass(worker.RetentionConfig{
		Pool:     pool,
		Leader:   leader,
		Store:    st,
		Objects:  objs,
		Outbox:   out,
		Metrics:  mtr,
		Logger:   zaptest.NewLogger(t),
		Interval: 50 * time.Millisecond,
		Batch:    100,
	})
	require.NoError(t, err)

	return &fixture{
		pool:   pool,
		store:  st,
		leader: leader,
		objs:   objs,
		out:    out,
		mtr:    mtr,
		pass:   pass,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Cold-move
// ─────────────────────────────────────────────────────────────────────────────

// TestRetentionPass_ColdMove_HappyPath seeds one due cold-move row,
// drives SweepOnce, and asserts:
//   - the row's status is now 'cold'
//   - exactly one recording.cold_moved audit row exists
//   - NO outbox event was appended (cold-move is metadata-only)
//   - the ObjectStore was NOT called.
func TestRetentionPass_ColdMove_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := seedTenant(t, f.pool)
	callID := seedCallInTenant(t, f.pool, tenantID)
	row := newRow(t, tenantID, callID)
	row.Status = "stored"
	row.ColdAt = now.Add(-1 * time.Hour) // due
	require.NoError(t, f.pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := f.store.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	require.NoError(t, f.pass.SweepOnce(t.Context()))

	got, err := f.store.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, "cold", got.Status, "row must be cold after cold-move sweep")

	require.Equal(t, 1, auditCount(t, f.pool, tenantID, rapi.AuditActionColdMoved),
		"exactly one cold_moved audit row")
	require.Empty(t, f.out.snapshot(), "cold-move must NOT emit an outbox event")
	require.Equal(t, 0, f.objs.deleteCount(), "cold-move must NOT call ObjectStore.Delete")

	// Metric label assertions: silent regressions in the label set
	// (e.g. accidentally sending "stale" instead of "ok") fail loudly here.
	tenantLabel := tenantID.String()
	require.InDelta(t, 1.0,
		counterValue(t, f.mtr.RetentionActionsTotal, tenantLabel, "cold_moved", "ok"),
		0, "RetentionActionsTotal{cold_moved,ok} must tick exactly once")
}

// ─────────────────────────────────────────────────────────────────────────────
// Hard-delete: happy path
// ─────────────────────────────────────────────────────────────────────────────

// TestRetentionPass_Delete_HappyPath seeds one due-delete row, runs the
// sweep, and asserts:
//   - ObjectStore.Delete was called once with the correct (bucket, key)
//   - the row's status is now 'deleted'
//   - one recording.deleted audit row exists
//   - one outbox event with the recording.call.deleted subject + payload.
func TestRetentionPass_Delete_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := seedTenant(t, f.pool)
	callID := seedCallInTenant(t, f.pool, tenantID)
	row := newRow(t, tenantID, callID)
	row.Status = "cold"
	past := now.Add(-1 * time.Hour)
	row.DeleteAt = &past // due
	require.NoError(t, f.pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := f.store.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	require.NoError(t, f.pass.SweepOnce(t.Context()))

	got, err := f.store.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, "deleted", got.Status, "row must be deleted after sweep")

	// ObjectStore.Delete called once with the row's bucket+key.
	require.Equal(t, 1, f.objs.deleteCount(), "ObjectStore.Delete called once")
	dc := f.objs.firstDelete()
	require.Equal(t, row.S3Bucket, dc.bucket)
	require.Equal(t, row.AudioObjectKey, dc.key)

	// Audit + outbox.
	require.Equal(t, 1, auditCount(t, f.pool, tenantID, rapi.AuditActionDeleted))
	events := f.out.snapshot()
	require.Len(t, events, 1, "exactly one outbox event for delete")
	ev := events[0]
	require.Equal(t, rapi.SubjectRecordingCallDeletedFor(tenantID), ev.Subject)
	require.NotNil(t, ev.TenantID)
	require.Equal(t, tenantID, *ev.TenantID)

	// Unmarshal the outbox payload and assert every field — guards
	// against JSON-tag drift on RecordingCallDeletedEvent that would
	// silently break downstream subscribers.
	var payload rapi.RecordingCallDeletedEvent
	require.NoError(t, json.Unmarshal(ev.Payload, &payload), "payload must round-trip")
	require.Equal(t, row.ID, payload.RecordingID, "payload.recording_id must equal row.ID")
	require.Equal(t, callID, payload.CallID, "payload.call_id must equal seeded call")
	require.Equal(t, tenantID, payload.TenantID, "payload.tenant_id must equal seeded tenant")
	require.Equal(t, "retention", payload.Reason,
		"payload.reason must be 'retention' for worker-driven deletes")
	require.Greater(t, payload.DeletedAt, int64(0),
		"payload.deleted_at must be a non-zero unix timestamp")

	// Metric label assertions.
	tenantLabel := tenantID.String()
	require.InDelta(t, 1.0,
		counterValue(t, f.mtr.RetentionActionsTotal, tenantLabel, "deleted", "ok"),
		0, "RetentionActionsTotal{deleted,ok} must tick exactly once on happy-path")
}

// ─────────────────────────────────────────────────────────────────────────────
// Hard-delete: ErrObjectNotFound (orphan reconciliation)
// ─────────────────────────────────────────────────────────────────────────────

// TestRetentionPass_Delete_ObjectAlreadyGone simulates the case where
// the audio object has already been purged (e.g. by a manual ops cleanup
// or a previous half-completed sweep). Phase A returns
// storage.ErrObjectNotFound; the worker must STILL flip the row to
// 'deleted' so DB and S3 stay consistent, AND emit the outbox event.
func TestRetentionPass_Delete_ObjectAlreadyGone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Stage the next Delete to return ErrObjectNotFound.
	f.objs.nextErr = storage.ErrObjectNotFound

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := seedTenant(t, f.pool)
	callID := seedCallInTenant(t, f.pool, tenantID)
	row := newRow(t, tenantID, callID)
	row.Status = "stored"
	past := now.Add(-1 * time.Hour)
	row.DeleteAt = &past
	require.NoError(t, f.pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := f.store.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	require.NoError(t, f.pass.SweepOnce(t.Context()))

	got, err := f.store.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, "deleted", got.Status,
		"orphan path must still flip status — DB and S3 reconciled")
	require.Equal(t, 1, auditCount(t, f.pool, tenantID, rapi.AuditActionDeleted))
	require.Len(t, f.out.snapshot(), 1, "outbox event still emitted on orphan path")

	// Metric label assertion: the orphan branch must record under the
	// dedicated "orphaned" result label so dashboards distinguish
	// reconciliation events from clean deletes.
	tenantLabel := tenantID.String()
	require.InDelta(t, 1.0,
		counterValue(t, f.mtr.RetentionActionsTotal, tenantLabel, "deleted", "orphaned"),
		0, "RetentionActionsTotal{deleted,orphaned} must tick on the orphan branch")
	require.InDelta(t, 0.0,
		counterValue(t, f.mtr.RetentionActionsTotal, tenantLabel, "deleted", "ok"),
		0, "the 'ok' label must NOT also tick — orphan path is mutually exclusive")
}

// ─────────────────────────────────────────────────────────────────────────────
// Hard-delete: generic ObjectStore error → no Phase B + retry next tick
// ─────────────────────────────────────────────────────────────────────────────

// TestRetentionPass_Delete_ObjectStoreError simulates a transient S3
// outage: Phase A returns a generic error, NOT ErrObjectNotFound. The
// worker must NOT flip status (the audio is still there; flipping the
// flag would orphan a real object) and must NOT emit the outbox event.
// The row stays eligible so the next sweep retries.
func TestRetentionPass_Delete_ObjectStoreError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.objs.nextErr = errors.New("simulated S3 5xx")

	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := seedTenant(t, f.pool)
	callID := seedCallInTenant(t, f.pool, tenantID)
	row := newRow(t, tenantID, callID)
	row.Status = "cold"
	past := now.Add(-1 * time.Hour)
	row.DeleteAt = &past
	require.NoError(t, f.pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := f.store.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))

	// One bad row must NOT poison the sweep — SweepOnce returns nil.
	require.NoError(t, f.pass.SweepOnce(t.Context()))

	got, err := f.store.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, "cold", got.Status, "Phase A failure must NOT flip status")
	require.Equal(t, 0, auditCount(t, f.pool, tenantID, rapi.AuditActionDeleted),
		"no audit row when Phase A failed")
	require.Empty(t, f.out.snapshot(), "no outbox event when Phase A failed")

	// Metric label assertion: a generic Phase A error MUST land on the
	// "error" result label (not "stale" or "orphaned"). This is the
	// signal operators alert on for transient S3 outages.
	tenantLabel := tenantID.String()
	require.InDelta(t, 1.0,
		counterValue(t, f.mtr.RetentionActionsTotal, tenantLabel, "deleted", "error"),
		0, "RetentionActionsTotal{deleted,error} must tick on Phase A failure")
}

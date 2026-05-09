//go:build integration

package worker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/internal/recording/worker"
	"github.com/sociopulse/platform/pkg/postgres"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fakes (specific to the integrity tests)
// ─────────────────────────────────────────────────────────────────────────────

// fakeIntegrityLeader is an always-leading Leader keyed on
// IntegrityLockKey. Mirrors fakeLeader from retention_test.go but uses
// the integrity slot so the two test files can run in the same package
// without aliasing the type.
type fakeIntegrityLeader struct {
	mu       sync.Mutex
	acquired int
	released int
}

func (f *fakeIntegrityLeader) Acquire(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired++
	return true, nil
}

func (f *fakeIntegrityLeader) Release(_ context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
}

func (f *fakeIntegrityLeader) Key() int64 { return worker.IntegrityLockKey }

// fakeRecordingService implements rapi.RecordingService just enough to
// drive VerifyChecksum from the integrity worker. The other methods
// return stub errors so an accidental call surfaces a clear test
// failure. Results are looked up by recording call_id; missing entries
// fall back to defaultResult / defaultErr.
type fakeRecordingService struct {
	mu            sync.Mutex
	results       map[uuid.UUID]rapi.VerifyResult
	errs          map[uuid.UUID]error
	defaultResult rapi.VerifyResult
	defaultErr    error
	verifyCalls   []uuid.UUID
}

// Compile-time check.
var _ rapi.RecordingService = (*fakeRecordingService)(nil)

func newFakeRecordingService() *fakeRecordingService {
	return &fakeRecordingService{
		results: make(map[uuid.UUID]rapi.VerifyResult),
		errs:    make(map[uuid.UUID]error),
	}
}

func (f *fakeRecordingService) setResult(callID uuid.UUID, r rapi.VerifyResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[callID] = r
}

func (f *fakeRecordingService) setError(callID uuid.UUID, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[callID] = err
}

func (f *fakeRecordingService) verifyCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.verifyCalls)
}

func (f *fakeRecordingService) Commit(_ context.Context, _ rapi.CommitInput) (rapi.CommitOutput, error) {
	return rapi.CommitOutput{}, errors.New("fakeRecordingService.Commit: not used by worker tests")
}

func (f *fakeRecordingService) Get(_ context.Context, _, _ uuid.UUID) (rapi.RecordingMetadata, error) {
	return rapi.RecordingMetadata{}, errors.New("fakeRecordingService.Get: not used by worker tests")
}

func (f *fakeRecordingService) Search(_ context.Context, _ uuid.UUID, _ rapi.SearchQuery) (rapi.SearchResult, error) {
	return rapi.SearchResult{}, errors.New("fakeRecordingService.Search: not used by worker tests")
}

func (f *fakeRecordingService) OpenAudioStream(_ context.Context, _, _ uuid.UUID, _ *rapi.ByteRange) (rapi.AudioStream, error) {
	return rapi.AudioStream{}, errors.New("fakeRecordingService.OpenAudioStream: not used by worker tests")
}

func (f *fakeRecordingService) VerifyChecksum(_ context.Context, _, callID uuid.UUID) (rapi.VerifyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyCalls = append(f.verifyCalls, callID)
	if err, ok := f.errs[callID]; ok && err != nil {
		return rapi.VerifyResult{}, err
	}
	if r, ok := f.results[callID]; ok {
		return r, nil
	}
	if f.defaultErr != nil {
		return rapi.VerifyResult{}, f.defaultErr
	}
	return f.defaultResult, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// counterValue reads the current value of a tenant-scoped result-label
// CounterVec cell. Returns 0 when the cell hasn't been touched yet.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labelValues ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

// histogramSampleCount reads the total sample count of a one-label
// HistogramVec cell. Returns 0 when the cell hasn't been touched yet.
func histogramSampleCount(t *testing.T, vec *prometheus.HistogramVec, labelValues ...string) uint64 {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	pm, ok := obs.(prometheus.Metric)
	require.True(t, ok, "histogram observer must satisfy prometheus.Metric")
	var m dto.Metric
	require.NoError(t, pm.Write(&m))
	return m.GetHistogram().GetSampleCount()
}

// readVerifyColumns fetches verified_at + integrity_ok directly via the
// store API. Both are nullable in the schema; callers assert nil-ness.
func readVerifyColumns(t *testing.T, st *store.PostgresStore, tenantID, callID uuid.UUID) (verifiedAt *time.Time, integrityOK *bool) {
	t.Helper()
	got, err := st.GetByCallID(t.Context(), tenantID, callID)
	require.NoError(t, err)
	return got.VerifiedAt, got.IntegrityOK
}

// ─────────────────────────────────────────────────────────────────────────────
// Integrity fixture builder
// ─────────────────────────────────────────────────────────────────────────────

type integrityFixture struct {
	pool   *postgres.Pool
	store  *store.PostgresStore
	leader *fakeIntegrityLeader
	svc    *fakeRecordingService
	mtr    *metrics.RecordingMetrics
	pass   *worker.IntegrityPass
}

func newIntegrityFixture(t *testing.T) *integrityFixture {
	t.Helper()
	pool := startPGContainer(t)
	st := store.NewPostgresStore(pool)
	leader := &fakeIntegrityLeader{}
	svc := newFakeRecordingService()
	reg := prometheus.NewRegistry()
	mtr, err := metrics.RegisterRecordingMetrics(reg)
	require.NoError(t, err)

	pass, err := worker.NewIntegrityPass(worker.IntegrityConfig{
		Pool:          pool,
		Leader:        leader,
		Store:         st,
		Service:       svc,
		Metrics:       mtr,
		Logger:        zaptest.NewLogger(t),
		Interval:      50 * time.Millisecond,
		Batch:         10,
		SamplePercent: 100, // exhaust eligible set in tests
	})
	require.NoError(t, err)

	return &integrityFixture{
		pool:   pool,
		store:  st,
		leader: leader,
		svc:    svc,
		mtr:    mtr,
		pass:   pass,
	}
}

// seedEligibleRow inserts one recording row that satisfies the
// SampleForVerify eligibility predicate (status='stored',
// verified_at IS NULL). Returns (tenantID, callID); the recording's
// surrogate id is not returned because every assertion the tests make
// works through the (tenantID, callID) tuple.
func seedEligibleRow(t *testing.T, f *integrityFixture) (tenantID, callID uuid.UUID) {
	t.Helper()
	tenantID = seedTenant(t, f.pool)
	callID = seedCallInTenant(t, f.pool, tenantID)
	row := newRow(t, tenantID, callID)
	row.Status = "stored"
	row.VerifiedAt = nil
	row.IntegrityOK = nil
	require.NoError(t, f.pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, _, err := f.store.InsertRecordingIdempotent(t.Context(), tx, row)
		return err
	}))
	return tenantID, callID
}

// ─────────────────────────────────────────────────────────────────────────────
// Happy path: VerifyResult OK=true → integrity_ok=true persisted, audit row, ok metric
// ─────────────────────────────────────────────────────────────────────────────

// TestIntegrityPass_HappyPath_OK seeds one eligible row, configures the
// fake service to return OK=true, drives SweepOnce, and asserts:
//   - verified_at is non-nil
//   - integrity_ok is non-nil and true
//   - exactly one recording.verified audit row exists
//   - IntegrityActionsTotal{tenant,ok} == 1
//   - IntegrityFailuresTotal{tenant} == 0 (no failure on success)
//   - IntegrityPassDuration{ok} sample count == 1.
func TestIntegrityPass_HappyPath_OK(t *testing.T) {
	t.Parallel()
	f := newIntegrityFixture(t)
	tenantID, callID := seedEligibleRow(t, f)

	expected := "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100"
	f.svc.setResult(callID, rapi.VerifyResult{
		OK:           true,
		ExpectedSHA:  expected,
		ActualSHA:    expected,
		BytesScanned: 1234567,
		DurationMS:   42,
	})

	require.NoError(t, f.pass.SweepOnce(t.Context()))

	require.Equal(t, 1, f.svc.verifyCallCount(), "VerifyChecksum called once")

	verifiedAt, ok := readVerifyColumns(t, f.store, tenantID, callID)
	require.NotNil(t, verifiedAt, "verified_at must be set after happy-path sweep")
	require.NotNil(t, ok)
	require.True(t, *ok, "integrity_ok must be true on OK result")

	require.Equal(t, 1, auditCount(t, f.pool, tenantID, rapi.AuditActionVerified),
		"exactly one recording.verified audit row")

	tenantLabel := tenantID.String()
	require.InDelta(t, 1.0, counterValue(t, f.mtr.IntegrityActionsTotal, tenantLabel, "ok"), 0)
	require.InDelta(t, 0.0, counterValue(t, f.mtr.IntegrityFailuresTotal, tenantLabel), 0)
	require.Equal(t, uint64(1), histogramSampleCount(t, f.mtr.IntegrityPassDuration, "ok"))
}

// ─────────────────────────────────────────────────────────────────────────────
// Mismatch path: VerifyResult OK=false → integrity_ok=false, IntegrityFailuresTotal++
// ─────────────────────────────────────────────────────────────────────────────

// TestIntegrityPass_HappyPath_Mismatch seeds one eligible row, configures
// the fake service to return OK=false (sha256 disagreement), drives
// SweepOnce, and asserts:
//   - verified_at is non-nil (chain-of-custody requires we record the check)
//   - integrity_ok is non-nil and false
//   - exactly one recording.verified audit row exists with the actual_sha
//   - IntegrityActionsTotal{tenant,mismatch} == 1
//   - IntegrityFailuresTotal{tenant} == 1 (master spec §15.5 alert metric).
func TestIntegrityPass_HappyPath_Mismatch(t *testing.T) {
	t.Parallel()
	f := newIntegrityFixture(t)
	tenantID, callID := seedEligibleRow(t, f)

	expected := "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100"
	actual := "0000000000000000000000000000000000000000000000000000000000000000"
	f.svc.setResult(callID, rapi.VerifyResult{
		OK:           false,
		ExpectedSHA:  expected,
		ActualSHA:    actual,
		BytesScanned: 1234567,
		DurationMS:   42,
	})

	require.NoError(t, f.pass.SweepOnce(t.Context()))

	verifiedAt, ok := readVerifyColumns(t, f.store, tenantID, callID)
	require.NotNil(t, verifiedAt, "verified_at must be set on mismatch — chain-of-custody record")
	require.NotNil(t, ok)
	require.False(t, *ok, "integrity_ok must be false on sha mismatch")

	require.Equal(t, 1, auditCount(t, f.pool, tenantID, rapi.AuditActionVerified),
		"recording.verified audit row written even on mismatch")

	tenantLabel := tenantID.String()
	require.InDelta(t, 1.0, counterValue(t, f.mtr.IntegrityActionsTotal, tenantLabel, "mismatch"), 0)
	require.InDelta(t, 1.0, counterValue(t, f.mtr.IntegrityFailuresTotal, tenantLabel), 0,
		"master spec §15.5 IntegrityFailuresTotal must tick on mismatch")
}

// ─────────────────────────────────────────────────────────────────────────────
// Transport error: VerifyChecksum returns error → no DB update, no audit, retry next sweep
// ─────────────────────────────────────────────────────────────────────────────

// TestIntegrityPass_TransportError_NoUpdate simulates a transient
// VerifyChecksum failure (S3 5xx, KMS outage, etc.). The worker MUST:
//   - NOT update verified_at (the row stays eligible — the next sweep retries)
//   - NOT write an audit row (no chain-of-custody record on a non-result)
//   - NOT bump IntegrityFailuresTotal (this isn't a confirmed mismatch)
//   - bump IntegrityActionsTotal{tenant,error}.
func TestIntegrityPass_TransportError_NoUpdate(t *testing.T) {
	t.Parallel()
	f := newIntegrityFixture(t)
	tenantID, callID := seedEligibleRow(t, f)

	f.svc.setError(callID, errors.New("simulated S3 5xx"))

	// One bad row must NOT poison the sweep — SweepOnce returns nil.
	require.NoError(t, f.pass.SweepOnce(t.Context()))

	verifiedAt, ok := readVerifyColumns(t, f.store, tenantID, callID)
	require.Nil(t, verifiedAt, "verified_at must NOT be set on transport error")
	require.Nil(t, ok, "integrity_ok must NOT be set on transport error")

	require.Equal(t, 0, auditCount(t, f.pool, tenantID, rapi.AuditActionVerified),
		"no audit row on transport error")

	tenantLabel := tenantID.String()
	require.InDelta(t, 1.0, counterValue(t, f.mtr.IntegrityActionsTotal, tenantLabel, "error"), 0)
	require.InDelta(t, 0.0, counterValue(t, f.mtr.IntegrityFailuresTotal, tenantLabel), 0,
		"transport error is NOT a confirmed mismatch — IntegrityFailuresTotal must stay 0")
}

// ─────────────────────────────────────────────────────────────────────────────
// Empty sample: SampleForVerify returns no rows → SweepOnce no-op
// ─────────────────────────────────────────────────────────────────────────────

// TestIntegrityPass_EmptySample_NoOp seeds NO rows. SampleForVerify must
// return an empty slice, the sweep must complete cleanly without calling
// VerifyChecksum, and the pass-duration histogram must still record one
// "ok" sample (so an alert on absent metric activity surfaces a hung
// daemon, not an empty queue).
func TestIntegrityPass_EmptySample_NoOp(t *testing.T) {
	t.Parallel()
	f := newIntegrityFixture(t)

	require.NoError(t, f.pass.SweepOnce(t.Context()))

	require.Equal(t, 0, f.svc.verifyCallCount(),
		"VerifyChecksum must NOT be called on an empty sample")
	require.Equal(t, uint64(1), histogramSampleCount(t, f.mtr.IntegrityPassDuration, "ok"),
		"pass-duration histogram must tick once even on empty sample")
}

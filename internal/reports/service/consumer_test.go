package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	storage "github.com/sociopulse/platform/internal/recording/storage"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	rptevents "github.com/sociopulse/platform/internal/reports/events"
	reportsvc "github.com/sociopulse/platform/internal/reports/service"
	reportstore "github.com/sociopulse/platform/internal/reports/store"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// ─── shared fixtures ─────────────────────────────────────────────────────────

var (
	consumerNow = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
)

func consumerNowFn() time.Time { return consumerNow }

// taskWithID forges an *asynq.Task with the supplied id baked into the
// ctx. The asynq library exposes GetTaskID(ctx) which only works when
// the ctx came from asynq.Server. We can't reach that internal context
// helper directly, so the consumer's "jobID = GetTaskID(ctx)" call in
// unit tests yields ("", false). That's fine for the tests below — the
// tenant + audit assertions don't depend on jobID matching the asynq
// id, and the integration test pack drives the real asynq Server.
func newTaskWithPayload(t *testing.T, in reportsapi.JobInput) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(in)
	require.NoError(t, err)
	return asynq.NewTask(reportsapi.TaskJobRun, payload)
}

// newConsumerJobInput builds a JobInput fixture. kind is parameterised
// (rather than hard-coded to KindOperatorEfficiency) so future tests
// covering other kinds reuse the helper unchanged. Current unit suite
// happens to only use KindOperatorEfficiency.
//
//nolint:unparam // kind is parameterised for forward callers.
func newConsumerJobInput(kind reportsapi.ReportKind, format reportsapi.ExportFormat, params map[string]any) reportsapi.JobInput {
	return reportsapi.JobInput{
		RenderInput: reportsapi.RenderInput{
			Kind:     kind,
			Format:   format,
			Params:   params,
			Window:   analyticsapi.Window{From: dataFrom, To: dataTo},
			TenantID: queueTenant,
			ActorID:  queueActor,
		},
		NotifyUserID: queueActor,
	}
}

// fakeConsumerPool implements QueuePool. Invokes fn with a zero
// postgres.Tx — the Consumer's closures call only the injected
// flip / audit / ready functions, none of which touch the Tx
// underneath. We capture the tenantID handed in so tests can assert
// the Consumer enters the correct tenant scope.
type fakeConsumerPool struct {
	mu      sync.Mutex
	tenants []uuid.UUID
	nextErr error // if set, all WithTenant calls return this without invoking fn
}

func (f *fakeConsumerPool) WithTenant(_ context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	f.tenants = append(f.tenants, tenantID)
	f.mu.Unlock()
	if f.nextErr != nil {
		return f.nextErr
	}
	return fn(postgres.Tx{})
}

var _ reportsvc.QueuePool = (*fakeConsumerPool)(nil)

// fakeOutboxRecorder satisfies BOTH AuditWriter and OutboxWriter — same
// Append signature, so one fake serves both ports. The recorded events
// let tests assert subject + payload shape.
type fakeOutboxRecorder struct {
	mu      sync.Mutex
	events  []outbox.Event
	nextErr error
}

func (f *fakeOutboxRecorder) Append(_ context.Context, _ postgres.Tx, ev outbox.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nextErr != nil {
		return f.nextErr
	}
	f.events = append(f.events, ev)
	return nil
}

var _ reportsvc.AuditWriter = (*fakeOutboxRecorder)(nil)
var _ rptevents.OutboxWriter = (*fakeOutboxRecorder)(nil)

// recordingObjectStore wraps storage.LocalObjectStore with a Put-call
// observer so tests can assert the bucket/key the Consumer chose.
type recordingObjectStore struct {
	*storage.LocalObjectStore

	mu       sync.Mutex
	putCalls []putCall
}

type putCall struct {
	bucket      string
	key         string
	contentType string
	bytesLen    int
}

func newRecordingObjectStore() *recordingObjectStore {
	return &recordingObjectStore{LocalObjectStore: storage.NewLocalObjectStore()}
}

func (r *recordingObjectStore) Put(ctx context.Context, bucket, key string, payload []byte, contentType string) error {
	r.mu.Lock()
	r.putCalls = append(r.putCalls, putCall{
		bucket: bucket, key: key, contentType: contentType, bytesLen: len(payload),
	})
	r.mu.Unlock()
	return r.LocalObjectStore.Put(ctx, bucket, key, payload, contentType)
}

// ─── flip fakes ──────────────────────────────────────────────────────────────

type recordingFlip struct {
	mu      sync.Mutex
	running int
	succ    int
	failed  int

	runErr    error
	succErr   error
	failedErr error
}

func (f *recordingFlip) markRunning(_ context.Context, _ postgres.Tx, _ string, _ time.Time) error {
	f.mu.Lock()
	f.running++
	f.mu.Unlock()
	return f.runErr
}

func (f *recordingFlip) markSucceeded(_ context.Context, _ postgres.Tx, _ string, _ time.Time, _ int64, _, _ string) error {
	f.mu.Lock()
	f.succ++
	f.mu.Unlock()
	return f.succErr
}

func (f *recordingFlip) markFailed(_ context.Context, _ postgres.Tx, _ string, _ time.Time, _ string) error {
	f.mu.Lock()
	f.failed++
	f.mu.Unlock()
	return f.failedErr
}

// ─── consumer builder ────────────────────────────────────────────────────────

type consumerHarness struct {
	consumer *reportsvc.Consumer
	pool     *fakeConsumerPool
	store    *recordingObjectStore
	audit    *fakeOutboxRecorder
	ready    *fakeOutboxRecorder
	flip     *recordingFlip
}

func buildConsumer(t *testing.T, analytics analyticsapi.ServiceRO) *consumerHarness {
	t.Helper()
	pool := &fakeConsumerPool{}
	objStore := newRecordingObjectStore()
	audit := &fakeOutboxRecorder{}
	ready := &fakeOutboxRecorder{}
	flip := &recordingFlip{}

	c := reportsvc.NewConsumer(reportsvc.ConsumerDeps{
		Server:      nil, // not used by handleJobRun (only by Run)
		Analytics:   analytics,
		Pool:        pool,
		ObjectStore: objStore,
		Audit:       reportsvc.NewAuditEmitter(audit),
		ReadyPub:    rptevents.NewReportReadyPublisher(ready),
		Bucket:      "sociopulse-test-reports",
		PresignTTL:  24 * time.Hour,
		Logger:      zaptest.NewLogger(t),
		Now:         consumerNowFn,
	})
	reportsvc.SetConsumerFlipsForTest(c, flip.markRunning, flip.markSucceeded, flip.markFailed)

	return &consumerHarness{
		consumer: c, pool: pool, store: objStore,
		audit: audit, ready: ready, flip: flip,
	}
}

// ─── handleJobRun: happy path ────────────────────────────────────────────────

func TestConsumer_HandleJobRun_HappyPath(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{
		comparisons: []analyticsapi.OperatorComparisonRow{{
			OperatorID:  uuid.MustParse("44444444-4444-4444-4444-444444444444"),
			DisplayName: "X",
		}},
	}
	h := buildConsumer(t, fa)
	task := newTaskWithPayload(t, newConsumerJobInput(reportsapi.KindOperatorEfficiency,
		reportsapi.FormatXLSX, map[string]any{"project_id": queueProject.String()}))

	err := reportsvc.HandleJobRunForTest(h.consumer, context.Background(), task)
	require.NoError(t, err)

	require.Equal(t, 1, h.flip.running, "MarkRunningTx called once")
	require.Equal(t, 1, h.flip.succ, "MarkSucceededTx called once")
	require.Equal(t, 0, h.flip.failed)

	// Pool entered WithTenant twice (Phase 1 + Phase 4), same tenant both times.
	require.Len(t, h.pool.tenants, 2)
	require.Equal(t, queueTenant, h.pool.tenants[0])
	require.Equal(t, queueTenant, h.pool.tenants[1])

	// Put + Presign happened: single shared bucket per environment
	// (cfg.S3.Buckets.Reports == "sociopulse-test-reports" in tests),
	// tenant isolation rides on the leading <tenant>/ component of the
	// key. Layout: "<tenant>/<kind>/<yyyy>/<mm>/<dd>/<actor>-<filename>".
	require.Len(t, h.store.putCalls, 1)
	require.Equal(t, "sociopulse-test-reports", h.store.putCalls[0].bucket)
	require.True(t, strings.HasPrefix(h.store.putCalls[0].key, queueTenant.String()+"/"),
		"key starts with the tenant UUID followed by '/'")
	require.Contains(t, h.store.putCalls[0].key, string(reportsapi.KindOperatorEfficiency))
	require.Contains(t, h.store.putCalls[0].key, "2026/05/14",
		"the synthetic key embeds the UTC date of c.d.Now()")
	require.Contains(t, h.store.putCalls[0].key, queueActor.String())
	require.Positive(t, h.store.putCalls[0].bytesLen, "Put receives the rendered bytes")

	// Audit + Ready outbox events both appended.
	require.Len(t, h.audit.events, 1)
	require.Equal(t, "tenant."+queueTenant.String()+".audit.event", h.audit.events[0].Subject)
	require.Len(t, h.ready.events, 1)
	require.Equal(t, "tenant."+queueTenant.String()+".reports.report.ready", h.ready.events[0].Subject)

	// ReportReadyEvent payload has the presigned URL (24h stub).
	var ev reportsapi.ReportReadyEvent
	require.NoError(t, json.Unmarshal(h.ready.events[0].Payload, &ev))
	require.NotEmpty(t, ev.DownloadURL)
	require.Equal(t, queueTenant.String(), ev.TenantID)
	require.Equal(t, string(reportsapi.KindOperatorEfficiency), ev.Kind)
	require.Equal(t, string(reportsapi.FormatXLSX), ev.Format)
}

// ─── handleJobRun: bad payload → SkipRetry ───────────────────────────────────

func TestConsumer_HandleJobRun_BadPayloadIsPermanent(t *testing.T) {
	t.Parallel()

	h := buildConsumer(t, &fakeAnalytics{})
	bad := asynq.NewTask(reportsapi.TaskJobRun, []byte("not-json-{"))

	err := reportsvc.HandleJobRunForTest(h.consumer, context.Background(), bad)
	require.Error(t, err)
	require.ErrorIs(t, err, asynq.SkipRetry, "bad payload must be permanent (SkipRetry)")
	require.Zero(t, h.flip.running, "MarkRunning must not be called on bad payload")
	require.Empty(t, h.audit.events)
}

// ─── handleJobRun: Phase 1 ErrStaleSkip → ack ────────────────────────────────

func TestConsumer_HandleJobRun_MarkRunningStaleSkipAcks(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{
		comparisons: []analyticsapi.OperatorComparisonRow{{OperatorID: uuid.New()}},
	}
	h := buildConsumer(t, fa)
	h.flip.runErr = reportstore.ErrStaleSkip

	task := newTaskWithPayload(t, newConsumerJobInput(reportsapi.KindOperatorEfficiency,
		reportsapi.FormatXLSX, map[string]any{"project_id": queueProject.String()}))

	err := reportsvc.HandleJobRunForTest(h.consumer, context.Background(), task)
	require.NoError(t, err, "ErrStaleSkip on MarkRunning must yield an ack (nil)")
	require.Equal(t, 1, h.flip.running)
	require.Zero(t, h.flip.succ, "must NOT proceed to render+succeed when row already terminal")
	require.Empty(t, h.store.putCalls, "must NOT touch S3 when ack-only")
}

// ─── handleJobRun: Phase 4 ErrStaleSkip → ack (canceled mid-render) ──────────

func TestConsumer_HandleJobRun_MarkSucceededStaleSkipAcks(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{
		comparisons: []analyticsapi.OperatorComparisonRow{{OperatorID: uuid.New()}},
	}
	h := buildConsumer(t, fa)
	h.flip.succErr = reportstore.ErrStaleSkip

	task := newTaskWithPayload(t, newConsumerJobInput(reportsapi.KindOperatorEfficiency,
		reportsapi.FormatXLSX, map[string]any{"project_id": queueProject.String()}))

	err := reportsvc.HandleJobRunForTest(h.consumer, context.Background(), task)
	require.NoError(t, err, "race: row canceled between Phase 1 and Phase 4 must ack")
	require.Equal(t, 1, h.flip.succ)
	// Audit and Ready must NOT be emitted when MarkSucceededTx stale-skips.
	require.Empty(t, h.audit.events, "no audit when MarkSucceededTx stale-skips")
	require.Empty(t, h.ready.events, "no ready event when MarkSucceededTx stale-skips")
}

// ─── handleJobRun: render error → markFailed permanent ───────────────────────

func TestConsumer_HandleJobRun_RenderInvalidParamsIsPermanent(t *testing.T) {
	t.Parallel()

	// Missing project_id triggers ErrInvalidParams from FetchOperatorEfficiency.
	fa := &fakeAnalytics{}
	h := buildConsumer(t, fa)
	task := newTaskWithPayload(t, newConsumerJobInput(reportsapi.KindOperatorEfficiency,
		reportsapi.FormatXLSX, map[string]any{})) // no project_id

	err := reportsvc.HandleJobRunForTest(h.consumer, context.Background(), task)
	require.Error(t, err)
	require.ErrorIs(t, err, reportsapi.ErrInvalidParams)
	require.ErrorIs(t, err, asynq.SkipRetry, "permanent failures must wrap SkipRetry")
	require.Equal(t, 1, h.flip.failed, "MarkFailedTx must run for permanent failures")
	require.Len(t, h.audit.events, 1, "audit emitted even on failure (export attempt)")
}

// ─── handleJobRun: render error → markFailed transient ───────────────────────

func TestConsumer_HandleJobRun_TransientErrorBubbles(t *testing.T) {
	t.Parallel()

	// Analytics error → not ErrInvalidParams / ErrUnknownKind / etc → transient.
	sentinel := errors.New("analytics-flaky")
	fa := &fakeAnalytics{err: sentinel}
	h := buildConsumer(t, fa)
	task := newTaskWithPayload(t, newConsumerJobInput(reportsapi.KindOperatorEfficiency,
		reportsapi.FormatXLSX, map[string]any{"project_id": queueProject.String()}))

	err := reportsvc.HandleJobRunForTest(h.consumer, context.Background(), task)
	require.Error(t, err)
	require.ErrorIs(t, err, sentinel)
	require.NotErrorIs(t, err, asynq.SkipRetry, "transient errors must NOT wrap SkipRetry — asynq retries")
	require.Equal(t, 1, h.flip.failed, "MarkFailedTx still runs to persist the error message")
}

// ─── handleJobRun: ObjectStore.Put error → markFailed transient ──────────────

func TestConsumer_HandleJobRun_ObjectStorePutErrorIsTransient(t *testing.T) {
	t.Parallel()

	fa := &fakeAnalytics{
		comparisons: []analyticsapi.OperatorComparisonRow{{OperatorID: uuid.New()}},
	}
	h := buildConsumer(t, fa)
	// Substitute the object store for one whose Put returns an error.
	failingStore := &failingObjectStore{err: errors.New("s3-down")}
	c := reportsvc.NewConsumer(reportsvc.ConsumerDeps{
		Analytics:   fa,
		Pool:        h.pool,
		ObjectStore: failingStore,
		Audit:       reportsvc.NewAuditEmitter(h.audit),
		ReadyPub:    rptevents.NewReportReadyPublisher(h.ready),
		Bucket:      "sociopulse-test-reports",
		PresignTTL:  24 * time.Hour,
		Logger:      zaptest.NewLogger(t),
		Now:         consumerNowFn,
	})
	reportsvc.SetConsumerFlipsForTest(c, h.flip.markRunning, h.flip.markSucceeded, h.flip.markFailed)

	task := newTaskWithPayload(t, newConsumerJobInput(reportsapi.KindOperatorEfficiency,
		reportsapi.FormatXLSX, map[string]any{"project_id": queueProject.String()}))

	err := reportsvc.HandleJobRunForTest(c, context.Background(), task)
	require.Error(t, err)
	require.NotErrorIs(t, err, asynq.SkipRetry, "S3 errors are transient — asynq retries")
	require.Contains(t, err.Error(), "upload")
	require.Zero(t, h.flip.succ)
	require.Equal(t, 1, h.flip.failed)
}

// failingObjectStore returns an error on every operation. Used to drive
// the upload/presign failure path.
type failingObjectStore struct {
	err error
}

func (f *failingObjectStore) Get(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, f.err
}

func (f *failingObjectStore) Delete(_ context.Context, _, _ string) error { return f.err }

func (f *failingObjectStore) Put(_ context.Context, _, _ string, _ []byte, _ string) error {
	return f.err
}

func (f *failingObjectStore) PresignedURL(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	return "", f.err
}

var _ storage.ObjectStore = (*failingObjectStore)(nil)

// ─── Run lifecycle ───────────────────────────────────────────────────────────

// fakeAsynqServer is a recording AsynqServer for Run lifecycle tests.
// Run blocks on a channel that Shutdown closes; this models the
// "graceful shutdown" of *asynq.Server (Run returns nil on shutdown).
type fakeAsynqServer struct {
	mu        sync.Mutex
	done      chan struct{}
	runCount  int
	shutdown  int
	runReturn error // optional: pre-arm a Run() return value
}

func newFakeAsynqServer() *fakeAsynqServer {
	return &fakeAsynqServer{done: make(chan struct{})}
}

func (s *fakeAsynqServer) Run(_ asynq.Handler) error {
	s.mu.Lock()
	s.runCount++
	s.mu.Unlock()
	<-s.done
	return s.runReturn
}

func (s *fakeAsynqServer) Shutdown() {
	s.mu.Lock()
	s.shutdown++
	s.mu.Unlock()
	close(s.done)
}

var _ reportsvc.AsynqServer = (*fakeAsynqServer)(nil)

func TestConsumer_Run_ShutdownOnCtxCancel(t *testing.T) {
	t.Parallel()

	srv := newFakeAsynqServer()
	c := reportsvc.NewConsumer(reportsvc.ConsumerDeps{
		Server:      srv,
		Analytics:   &fakeAnalytics{},
		Pool:        &fakeConsumerPool{},
		ObjectStore: storage.NewLocalObjectStore(),
		Audit:       reportsvc.NewAuditEmitter(&fakeOutboxRecorder{}),
		ReadyPub:    rptevents.NewReportReadyPublisher(&fakeOutboxRecorder{}),
		Bucket:      "sociopulse-test-reports",
		PresignTTL:  24 * time.Hour,
		Logger:      zaptest.NewLogger(t),
		Now:         consumerNowFn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Give the goroutine a moment to enter Server.Run.
	require.Eventually(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.runCount == 1
	}, 2*time.Second, 5*time.Millisecond)

	cancel()

	select {
	case err := <-runDone:
		require.NoError(t, err, "Run returns nil on graceful shutdown")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Equal(t, 1, srv.shutdown, "Shutdown must be called exactly once")
}

func TestConsumer_Run_PropagatesServerError(t *testing.T) {
	t.Parallel()

	srv := newFakeAsynqServer()
	sentinel := errors.New("asynq-blew-up")
	srv.runReturn = sentinel

	c := reportsvc.NewConsumer(reportsvc.ConsumerDeps{
		Server:      srv,
		Analytics:   &fakeAnalytics{},
		Pool:        &fakeConsumerPool{},
		ObjectStore: storage.NewLocalObjectStore(),
		Audit:       reportsvc.NewAuditEmitter(&fakeOutboxRecorder{}),
		ReadyPub:    rptevents.NewReportReadyPublisher(&fakeOutboxRecorder{}),
		Bucket:      "sociopulse-test-reports",
		PresignTTL:  24 * time.Hour,
		Logger:      zaptest.NewLogger(t),
		Now:         consumerNowFn,
	})

	ctx := context.Background()
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Simulate Server.Run exiting on its own (e.g., Redis disconnect).
	close(srv.done)

	select {
	case err := <-runDone:
		require.Error(t, err)
		require.ErrorIs(t, err, sentinel)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Server.Run error")
	}
}

// ─── compile-time sanity ─────────────────────────────────────────────────────

func TestConsumer_ZapLoggerWired(t *testing.T) {
	t.Parallel()
	// Compile-time: Logger field of ConsumerDeps is *zap.Logger.
	_ = zap.Logger{}
	// Spot-check the harness can render the kind constant unambiguously.
	require.True(t, strings.HasPrefix(string(reportsapi.KindOperatorEfficiency), "operator"))
}

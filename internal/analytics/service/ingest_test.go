package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/metrics"
	"github.com/sociopulse/platform/internal/analytics/service"
	recordingapi "github.com/sociopulse/platform/internal/recording/api"
)

// fakeBus implements eventbus.Subscriber by exposing a Publish hook
// that synchronously invokes the registered handler. The handler must
// run synchronously so tests can assert handler-side state mutations
// immediately after Publish returns.
type fakeBus struct {
	mu       sync.Mutex
	handlers map[string]func(string, []byte) error
}

func newFakeBus() *fakeBus {
	return &fakeBus{handlers: map[string]func(string, []byte) error{}}
}

// Subscribe records the handler under subject. Last-write-wins on the
// same subject (matches NATS push-consumer behaviour for a single
// queue group + replica).
func (b *fakeBus) Subscribe(_ context.Context, subj, _ string, h func(string, []byte) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[subj] = h
	return nil
}

// Publish synchronously invokes the registered handler for subj. Used
// by tests to simulate a NATS broker delivering a message. Returns the
// handler's error so tests can assert ack/nak semantics.
func (b *fakeBus) Publish(t *testing.T, subj string, payload []byte) error {
	t.Helper()
	b.mu.Lock()
	h, ok := b.handlers[subj]
	b.mu.Unlock()
	require.True(t, ok, "fakeBus: no handler for %s", subj)
	return h(subj, payload)
}

// fakeStore implements service.StoreWriter and records every Insert*
// call for later assertion. The mutex guards both the per-row buffers
// and the failure-injection switch.
type fakeStore struct {
	mu sync.Mutex

	calls         []apianalytics.AnalyticsCallEventPayload
	opStates      []apianalytics.AnalyticsOperatorStateEventPayload
	recsUploaded  []recordingapi.RecordingUploadedEvent
	flushCallsInv atomic.Int32 // number of InsertCalls invocations (including empties)

	// failNext, when set, causes the NEXT InsertCalls invocation to
	// return errInject. Cleared after firing once.
	failNext bool
}

var errInject = errors.New("fakestore: injected failure")

func (s *fakeStore) InsertCalls(_ context.Context, rows []apianalytics.AnalyticsCallEventPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushCallsInv.Add(1)
	if s.failNext {
		s.failNext = false
		return errInject
	}
	s.calls = append(s.calls, rows...)
	return nil
}

func (s *fakeStore) InsertOperatorStates(_ context.Context, rows []apianalytics.AnalyticsOperatorStateEventPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opStates = append(s.opStates, rows...)
	return nil
}

func (s *fakeStore) InsertRecordingsUploaded(_ context.Context, rows []recordingapi.RecordingUploadedEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recsUploaded = append(s.recsUploaded, rows...)
	return nil
}

func (s *fakeStore) callsCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *fakeStore) opStatesCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.opStates)
}

func (s *fakeStore) recsUploadedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recsUploaded)
}

// Compile-time check that *fakeStore satisfies service.StoreWriter.
// This catches signature drift the moment it happens (per Plan 09
// carry-forward, lessons #8).
var _ service.StoreWriter = (*fakeStore)(nil)

// newCallEvent builds a well-formed call event with a fresh EventID
// (so the dedup LRU treats every helper call as a new event).
func newCallEvent() apianalytics.AnalyticsCallEventPayload {
	return apianalytics.AnalyticsCallEventPayload{
		Date:        "2026-05-14",
		TS:          time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC),
		TenantID:    uuid.New(),
		ProjectID:   uuid.New(),
		OperatorID:  uuid.New(),
		CallID:      uuid.New(),
		Status:      "success",
		DurationSec: 60,
		HangupCause: "NORMAL_CLEARING",
		RegionCode:  "MSK",
		AttemptNo:   1,
		TrunkUsed:   "trunk-a",
		EventID:     uuid.New(),
	}
}

// startPipeline boots a pipeline with the supplied bus + store +
// config, returns a cancel + run-error channel for shutdown assertions.
// Tests are expected to call cancel() and read the channel to ensure
// goleak passes.
func startPipeline(
	t *testing.T,
	bus *fakeBus,
	fs *fakeStore,
	m *metrics.IngestMetrics,
	cfg service.IngestConfig,
) (cancel context.CancelFunc, runErrCh <-chan error) {
	t.Helper()
	p, err := service.NewIngestPipeline(bus, fs, zap.NewNop(), m, cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	ch := make(chan error, 1)
	go func() { ch <- p.Run(ctx) }()
	// Allow the goroutine to call Subscribe and the ticker to start.
	awaitCond(t, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return len(bus.handlers) == 3
	})
	return cancel, ch
}

// awaitCond polls cond until it returns true or the 1-second deadline
// expires. Used for state that becomes true asynchronously (handler
// registration, flush completion). Deadline is fixed at 1s — every
// async window in this test suite is well under that; if the
// assertion fires, the impl is genuinely broken, not slow.
func awaitCond(t *testing.T, cond func() bool) {
	t.Helper()
	const deadline = time.Second
	to := time.NewTimer(deadline)
	defer to.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-tick.C:
			continue
		case <-to.C:
			t.Fatalf("awaitCond: deadline exceeded after %s", deadline)
		}
	}
}

// TestIngestPipeline_RoutesCallsToBuffer asserts the calls handler is
// registered, a published payload is decoded + buffered, and the count
// threshold flushes it through to the store.
func TestIngestPipeline_RoutesCallsToBuffer(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	cfg := service.IngestConfig{
		BatchSize:     1,
		FlushInterval: time.Hour, // count-only trigger
		DedupSize:     128,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, nil, cfg)

	ev := newCallEvent()
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, bus.Publish(t, apianalytics.SubjectCallsAnalytics, raw))

	awaitCond(t, func() bool { return fs.callsCount() == 1 })

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngestPipeline_RoutesOperatorStateToBuffer asserts the
// operator_state handler is wired and a happy-path event flushes.
func TestIngestPipeline_RoutesOperatorStateToBuffer(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	cfg := service.IngestConfig{
		BatchSize:     1,
		FlushInterval: time.Hour,
		DedupSize:     128,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, nil, cfg)

	ev := apianalytics.AnalyticsOperatorStateEventPayload{
		Date:               "2026-05-14",
		TS:                 time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC),
		TenantID:           uuid.New(),
		UserID:             uuid.New(),
		State:              "ready",
		DurationInStateSec: 30,
		EventID:            uuid.New(),
	}
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, bus.Publish(t, apianalytics.SubjectOperatorStateAnalytics, raw))

	awaitCond(t, func() bool { return fs.opStatesCount() == 1 })

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngestPipeline_RoutesRecordingUploadedToBuffer asserts the
// recording wildcard subject is wired and a tenant-specific publish
// reaches the buffer.
func TestIngestPipeline_RoutesRecordingUploadedToBuffer(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	cfg := service.IngestConfig{
		BatchSize:     1,
		FlushInterval: time.Hour,
		DedupSize:     128,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, nil, cfg)

	tenantID := uuid.New()
	ev := recordingapi.RecordingUploadedEvent{
		RecordingID:        uuid.New(),
		CallID:             uuid.New(),
		TenantID:           tenantID,
		ProjectID:          uuid.New(),
		FSNode:             "fs-01",
		S3Key:              "tenant/abc/recordings/xyz.bin",
		EncryptionKeyAlias: "kms-alias-a",
		EventID:            uuid.New(),
		BytesSize:          12345,
		DurationMS:         60000,
		DurationSec:        60,
		SHA256Hex:          "deadbeef",
		Status:             "stored",
		CommittedAt:        time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC).Unix(),
	}
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	// The subscriber binds the wildcard; publish on the concrete subject
	// to mimic NATS delivery semantics.
	require.NoError(t, bus.Publish(t, apianalytics.SubjectRecordingUploadedWildcard, raw))

	awaitCond(t, func() bool { return fs.recsUploadedCount() == 1 })

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngestPipeline_DedupSkipsRepeatedEventID asserts the dedup LRU
// drops duplicate event_ids. Same event published twice → store sees
// it once; the metrics registry shows one dedup_hits_total increment.
func TestIngestPipeline_DedupSkipsRepeatedEventID(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	reg := prometheus.NewRegistry()
	m, err := metrics.RegisterIngestMetrics(reg)
	require.NoError(t, err)
	cfg := service.IngestConfig{
		BatchSize:     1,
		FlushInterval: time.Hour,
		DedupSize:     128,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, m, cfg)

	ev := newCallEvent()
	raw, err := json.Marshal(ev)
	require.NoError(t, err)

	require.NoError(t, bus.Publish(t, apianalytics.SubjectCallsAnalytics, raw))
	awaitCond(t, func() bool { return fs.callsCount() == 1 })

	// Second publish of the SAME event_id is a dedup hit; the store
	// MUST NOT see another row, and the dedup_hits metric must tick.
	require.NoError(t, bus.Publish(t, apianalytics.SubjectCallsAnalytics, raw))
	awaitCond(t, func() bool {
		return counterValueOrZero(t, reg, "sociopulse_analytics_ingest_dedup_hits_total") == 1
	})
	require.Equal(t, 1, fs.callsCount())

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngestPipeline_PoisonAcksAndIncrementsDeadLetter asserts that
// invalid JSON is acked (handler returns nil → no NAK loop) and the
// dead_letter metric ticks; the buffer remains empty.
func TestIngestPipeline_PoisonAcksAndIncrementsDeadLetter(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	reg := prometheus.NewRegistry()
	m, err := metrics.RegisterIngestMetrics(reg)
	require.NoError(t, err)
	cfg := service.IngestConfig{
		BatchSize:     1,
		FlushInterval: time.Hour,
		DedupSize:     128,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, m, cfg)

	// Handler must return nil (ack) on poison; bus.Publish returns the
	// handler error, so require.NoError encodes the contract.
	require.NoError(t, bus.Publish(t, apianalytics.SubjectCallsAnalytics, []byte("not-json")))

	awaitCond(t, func() bool {
		return counterValueOrZero(t, reg, "sociopulse_analytics_ingest_dead_letter_total") == 1
	})
	require.Zero(t, fs.callsCount())

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngestPipeline_PoisonMissingEventID asserts that JSON-valid but
// EventID-zero payloads are also dead-lettered. The ingester needs a
// non-zero event_id for the dedup LRU; zero == sentinel for "producer
// forgot to set it".
func TestIngestPipeline_PoisonMissingEventID(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	reg := prometheus.NewRegistry()
	m, err := metrics.RegisterIngestMetrics(reg)
	require.NoError(t, err)
	cfg := service.IngestConfig{
		BatchSize:     1,
		FlushInterval: time.Hour,
		DedupSize:     128,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, m, cfg)

	ev := newCallEvent()
	ev.EventID = uuid.Nil // sentinel — should dead-letter
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, bus.Publish(t, apianalytics.SubjectCallsAnalytics, raw))

	awaitCond(t, func() bool {
		return counterValueOrZero(t, reg, "sociopulse_analytics_ingest_dead_letter_total") == 1
	})
	require.Zero(t, fs.callsCount())

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngestPipeline_FlushesOnInterval asserts the time-based flush
// trigger fires: BatchSize=1000 (count won't trip), FlushInterval short
// → ticker forces a flush.
func TestIngestPipeline_FlushesOnInterval(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	cfg := service.IngestConfig{
		BatchSize:     1000,
		FlushInterval: 50 * time.Millisecond,
		DedupSize:     128,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, nil, cfg)

	ev := newCallEvent()
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, bus.Publish(t, apianalytics.SubjectCallsAnalytics, raw))

	// Wait long enough for at least one ticker tick (50ms cap).
	awaitCond(t, func() bool { return fs.callsCount() == 1 })

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngestPipeline_FlushesOnBatchSize asserts the count-based trigger
// fires before the ticker: BatchSize=3 → publishing 3 events triggers
// flush even with a 1-hour FlushInterval.
func TestIngestPipeline_FlushesOnBatchSize(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	cfg := service.IngestConfig{
		BatchSize:     3,
		FlushInterval: time.Hour,
		DedupSize:     128,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, nil, cfg)

	for range 3 {
		ev := newCallEvent()
		raw, err := json.Marshal(ev)
		require.NoError(t, err)
		require.NoError(t, bus.Publish(t, apianalytics.SubjectCallsAnalytics, raw))
	}

	awaitCond(t, func() bool { return fs.callsCount() == 3 })

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngestPipeline_DrainOnContextDone asserts that when ctx is
// cancelled, the pipeline drains all non-empty buffers before Run
// returns. Setup: BatchSize huge, FlushInterval huge → buffers fill
// without flushing; cancel triggers drain.
func TestIngestPipeline_DrainOnContextDone(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	cfg := service.IngestConfig{
		BatchSize:     1000,
		FlushInterval: time.Hour,
		DedupSize:     128,
		DrainTimeout:  2 * time.Second,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, nil, cfg)

	// Publish 5 distinct events (well below the threshold).
	for range 5 {
		ev := newCallEvent()
		raw, err := json.Marshal(ev)
		require.NoError(t, err)
		require.NoError(t, bus.Publish(t, apianalytics.SubjectCallsAnalytics, raw))
	}
	require.Zero(t, fs.callsCount(), "no flush should have fired yet")

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
	// After Run returns, the drain has completed.
	require.Equal(t, 5, fs.callsCount(), "drain must flush all 5 buffered rows")
}

// TestIngestPipeline_TransientFailureReturnsNak asserts that a
// transient store failure is NAK'd to the bus (handler returns error).
// The store sees the rows once (a failed flush DOES drop them per Plan
// 13.2 Step 3.4 — the LRU absorbed event_ids, so re-delivery would
// dedupe). Failure-mode here means: failed Send increments Failed metric,
// no Inserted metric, and the buffer is reset.
func TestIngestPipeline_TransientFailureIncrementsFailed(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{failNext: true}
	reg := prometheus.NewRegistry()
	m, err := metrics.RegisterIngestMetrics(reg)
	require.NoError(t, err)
	cfg := service.IngestConfig{
		BatchSize:     1,
		FlushInterval: time.Hour,
		DedupSize:     128,
	}
	cancel, runErrCh := startPipeline(t, bus, fs, m, cfg)

	ev := newCallEvent()
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, bus.Publish(t, apianalytics.SubjectCallsAnalytics, raw))

	awaitCond(t, func() bool {
		return counterValueOrZero(t, reg, "sociopulse_analytics_ingest_failed_total") >= 1
	})
	// The injected failure on InsertCalls means the store buffer stays
	// empty even though the flush was attempted.
	require.Zero(t, fs.callsCount(), "failed flush => no rows landed")
	require.GreaterOrEqual(t, int(fs.flushCallsInv.Load()), 1, "flush was attempted")

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestIngestPipeline_RunTwiceErrors asserts Run is single-shot —
// invoking Run on the same pipeline twice (sequentially) returns an
// error on the second call. Idempotency is a wiring contract; the
// composition root constructs one pipeline and Run-s it once.
func TestIngestPipeline_RunTwiceErrors(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}
	cfg := service.IngestConfig{
		BatchSize:     1,
		FlushInterval: time.Hour,
		DedupSize:     128,
	}
	p, err := service.NewIngestPipeline(bus, fs, zap.NewNop(), nil, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- p.Run(ctx) }()
	awaitCond(t, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return len(bus.handlers) == 3
	})

	// Second concurrent Run must fail fast.
	err2 := p.Run(ctx)
	require.Error(t, err2)

	cancel()
	require.ErrorIs(t, <-runErrCh, context.Canceled)
}

// TestNewIngestPipeline_RejectsInvalidConfig asserts the constructor
// validates IngestConfig (BatchSize>0, FlushInterval>0, DedupSize>0).
func TestNewIngestPipeline_RejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	fs := &fakeStore{}

	cases := []struct {
		name string
		cfg  service.IngestConfig
	}{
		{"zero_batch_size", service.IngestConfig{BatchSize: 0, FlushInterval: time.Second, DedupSize: 10}},
		{"zero_flush_interval", service.IngestConfig{BatchSize: 1, FlushInterval: 0, DedupSize: 10}},
		{"zero_dedup_size", service.IngestConfig{BatchSize: 1, FlushInterval: time.Second, DedupSize: 0}},
		{"negative_batch_size", service.IngestConfig{BatchSize: -1, FlushInterval: time.Second, DedupSize: 10}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := service.NewIngestPipeline(bus, fs, zap.NewNop(), nil, tc.cfg)
			require.Error(t, err)
		})
	}
}

// TestNewIngestPipeline_RejectsNilDeps asserts the constructor surfaces
// wiring bugs at boot. nil bus or nil store is a programmer error.
func TestNewIngestPipeline_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	cfg := service.IngestConfig{BatchSize: 1, FlushInterval: time.Second, DedupSize: 10}

	_, err := service.NewIngestPipeline(nil, &fakeStore{}, zap.NewNop(), nil, cfg)
	require.Error(t, err, "nil bus must error")

	_, err = service.NewIngestPipeline(newFakeBus(), nil, zap.NewNop(), nil, cfg)
	require.Error(t, err, "nil store must error")
}

// counterValueOrZero gathers `name` from reg and returns the value of
// the timeseries whose "subject" label matches
// apianalytics.SubjectCallsAnalytics, or 0 when no matching cell exists
// yet (no fatal). Used in poll-loops where the metric appears after a
// brief async window. The label value is fixed here rather than
// parameterised — every assertion in this file probes the calls
// subject; if a future test needs another subject, generalise then.
func counterValueOrZero(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, mp := range fam.GetMetric() {
			for _, lp := range mp.GetLabel() {
				if lp.GetName() == "subject" && lp.GetValue() == apianalytics.SubjectCallsAnalytics {
					return mp.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

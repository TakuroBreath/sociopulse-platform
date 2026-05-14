package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/metrics"
	"github.com/sociopulse/platform/internal/analytics/store"
	recordingapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// ErrInvalidConfig is returned by IngestConfig.Validate when one of the
// required fields is zero / negative. Wraps fmt.Errorf chains so
// callers can errors.Is against the sentinel rather than match on
// message text.
var ErrInvalidConfig = errors.New("analytics/service: invalid ingest config")

// Default values applied by IngestConfig.applyDefaults when a field is
// left at its zero value. The QueueGroup default matches the spec; the
// drain timeout matches the project-wide /readyz timeout convention.
const (
	defaultQueueGroup   = "analytics-ingest"
	defaultDrainTimeout = 5 * time.Second
)

// StoreWriter is the narrow output port the IngestPipeline depends on.
// Lives in the consumer package per "accept interfaces, return structs"
// (golang-structs-interfaces skill).
//
// Production: *StoreAdapter (defined below) satisfies it by delegating
// to the package-level store.Insert* helpers in internal/analytics/store.
// Tests: a fakeStoreWriter records calls for assertion (see ingest_test.go).
//
// Each method takes a slice of rows; the empty-slice fast path is
// already implemented by the store package (returns nil without
// touching the driver), so the pipeline's flushAll can call these
// freely even when the buffer is empty.
type StoreWriter interface {
	InsertCalls(ctx context.Context, rows []apianalytics.AnalyticsCallEventPayload) error
	InsertOperatorStates(ctx context.Context, rows []apianalytics.AnalyticsOperatorStateEventPayload) error
	InsertRecordingsUploaded(ctx context.Context, rows []recordingapi.RecordingUploadedEvent) error
}

// StoreAdapter wraps a *store.Conn and dispatches to the package-level
// store.Insert* helpers. The production composition root constructs one
// of these and passes it to NewIngestPipeline. Keeping the store
// package free-standing (functions, not methods on Conn) lets the
// schema-shape tests in internal/analytics/store stay independent of
// any port-interface drift here.
type StoreAdapter struct {
	Conn *store.Conn
}

// Compile-time interface check: *StoreAdapter must satisfy StoreWriter.
// Catches signature drift the moment it happens.
var _ StoreWriter = (*StoreAdapter)(nil)

// InsertCalls delegates to store.InsertCalls.
func (a *StoreAdapter) InsertCalls(ctx context.Context, rows []apianalytics.AnalyticsCallEventPayload) error {
	return store.InsertCalls(ctx, a.Conn, rows)
}

// InsertOperatorStates delegates to store.InsertOperatorStates.
func (a *StoreAdapter) InsertOperatorStates(ctx context.Context, rows []apianalytics.AnalyticsOperatorStateEventPayload) error {
	return store.InsertOperatorStates(ctx, a.Conn, rows)
}

// InsertRecordingsUploaded delegates to store.InsertRecordingsUploaded.
func (a *StoreAdapter) InsertRecordingsUploaded(ctx context.Context, rows []recordingapi.RecordingUploadedEvent) error {
	return store.InsertRecordingsUploaded(ctx, a.Conn, rows)
}

// IngestConfig is the validated runtime configuration for an
// IngestPipeline. All four required fields must be > 0.
type IngestConfig struct {
	// BatchSize is the per-subject row-count threshold that triggers a
	// flush. The pipeline flushes whichever buffer crosses the
	// threshold first (not all three at once). Must be > 0.
	BatchSize int

	// FlushInterval is the wall-clock cadence at which the pipeline
	// force-flushes every non-empty buffer regardless of count. Must
	// be > 0. Typical production value: 5s.
	FlushInterval time.Duration

	// DedupSize is the per-subject DedupLRU capacity. Must be > 0.
	// Typical production value: 100_000 — keeps ~24h of event_ids in
	// memory at the spec's 1k events/min cap.
	DedupSize int

	// QueueGroup is the NATS push-consumer queue group. Multiple
	// replicas in the same group share the message stream; multiple
	// groups (one per replica) is queue-group degeneration (every
	// replica sees every message). Defaults to "analytics-ingest".
	QueueGroup string

	// DrainTimeout caps the time spent in flushAll during ctx.Done
	// drain. Defaults to 5s. Set explicitly if your CH is slow.
	DrainTimeout time.Duration
}

// Validate returns nil iff every required field is set. Failures wrap
// ErrInvalidConfig so callers can errors.Is against the sentinel.
func (c IngestConfig) Validate() error {
	if c.BatchSize <= 0 {
		return fmt.Errorf("%w: BatchSize must be > 0 (got %d)", ErrInvalidConfig, c.BatchSize)
	}
	if c.FlushInterval <= 0 {
		return fmt.Errorf("%w: FlushInterval must be > 0 (got %s)", ErrInvalidConfig, c.FlushInterval)
	}
	if c.DedupSize <= 0 {
		return fmt.Errorf("%w: DedupSize must be > 0 (got %d)", ErrInvalidConfig, c.DedupSize)
	}
	return nil
}

// applyDefaults fills nil-tolerated fields with their defaults. The
// caller must call Validate FIRST to surface required-field gaps.
func (c *IngestConfig) applyDefaults() {
	if c.QueueGroup == "" {
		c.QueueGroup = defaultQueueGroup
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = defaultDrainTimeout
	}
}

// IngestPipeline drains the three analytics-bound subjects
// (analytics.event.calls, analytics.event.operator_state, and the
// per-tenant tenant.*.recording.uploaded wildcard) into ClickHouse via
// per-subject batched inserts.
//
// Concurrency model:
//   - 3 subscribers registered on a shared queue group; each handler
//     runs on the bus's push-dispatcher goroutine.
//   - 1 ticker goroutine forces a flush every FlushInterval.
//   - 1 shared mutex (mu) guards all 3 buffers + LRUs. Contention is
//     low: the workload is bounded to ~100 msg/s in v1; the lock is
//     held for buffer append + dedup probe (microseconds).
//
// Lifecycle:
//   - Run is single-shot. A second concurrent Run returns an error.
//   - ctx.Done triggers a drain: a fresh context with DrainTimeout is
//     used so the drain CAN complete even though the parent ctx is
//     cancelled.
//   - Run returns ctx.Err (typically context.Canceled) after drain.
type IngestPipeline struct {
	bus     eventbus.Subscriber
	store   StoreWriter
	logger  *zap.Logger
	metrics *metrics.IngestMetrics
	cfg     IngestConfig

	mu             sync.Mutex // guards the three subject buffers (the LRUs have their own internal mutex)
	callsBuf       []apianalytics.AnalyticsCallEventPayload
	opStateBuf     []apianalytics.AnalyticsOperatorStateEventPayload
	recUploadedBuf []recordingapi.RecordingUploadedEvent

	callsLRU       *DedupLRU
	opStateLRU     *DedupLRU
	recUploadedLRU *DedupLRU

	// runMu serialises Run invocations. running flips to true on entry
	// and stays true even after Run returns — second Run is rejected
	// for the lifetime of the pipeline.
	//
	// runCtx is captured on entry to Run and used as the ancestor for
	// count-threshold flushes invoked from handlers (which themselves
	// receive no ctx from the bus). Wrapped in context.WithoutCancel at
	// the call site so a parent cancellation does not abort an
	// in-flight flush. Reads from handler goroutines are safe because
	// runCtx is assigned BEFORE p.bus.Subscribe registers any handler.
	runMu   sync.Mutex
	running bool
	runCtx  context.Context
}

// NewIngestPipeline constructs a pipeline. bus and store MUST be
// non-nil — passing nil is a wiring bug. logger nil-falls-back to a
// nop. m nil-safe (every Inc* helper short-circuits on nil).
//
// cfg.Validate is run synchronously; an invalid config surfaces here,
// at boot, not at Run time.
func NewIngestPipeline(
	bus eventbus.Subscriber,
	store StoreWriter,
	logger *zap.Logger,
	m *metrics.IngestMetrics,
	cfg IngestConfig,
) (*IngestPipeline, error) {
	if bus == nil {
		return nil, errors.New("analytics/service: NewIngestPipeline: bus must be non-nil")
	}
	if store == nil {
		return nil, errors.New("analytics/service: NewIngestPipeline: store must be non-nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if logger == nil {
		logger = zap.NewNop()
	}

	return &IngestPipeline{
		bus:            bus,
		store:          store,
		logger:         logger,
		metrics:        m,
		cfg:            cfg,
		callsBuf:       make([]apianalytics.AnalyticsCallEventPayload, 0, cfg.BatchSize),
		opStateBuf:     make([]apianalytics.AnalyticsOperatorStateEventPayload, 0, cfg.BatchSize),
		recUploadedBuf: make([]recordingapi.RecordingUploadedEvent, 0, cfg.BatchSize),
		callsLRU:       NewDedupLRU(cfg.DedupSize),
		opStateLRU:     NewDedupLRU(cfg.DedupSize),
		recUploadedLRU: NewDedupLRU(cfg.DedupSize),
	}, nil
}

// Run registers the three subject subscribers and blocks until ctx is
// cancelled. On cancellation it drains every non-empty buffer through
// the store under a detached DrainTimeout context, then returns
// ctx.Err.
//
// Idempotency: Run is single-shot. A second Run (concurrent or
// sequential) returns an error. The composition root constructs one
// pipeline and Run-s it once.
func (p *IngestPipeline) Run(ctx context.Context) error {
	p.runMu.Lock()
	if p.running {
		p.runMu.Unlock()
		return errors.New("analytics/service: IngestPipeline.Run called more than once")
	}
	p.running = true
	p.runCtx = ctx
	p.runMu.Unlock()

	if err := p.bus.Subscribe(ctx, apianalytics.SubjectCallsAnalytics, p.cfg.QueueGroup, p.handleCalls); err != nil {
		return fmt.Errorf("analytics/service: subscribe %q: %w", apianalytics.SubjectCallsAnalytics, err)
	}
	if err := p.bus.Subscribe(ctx, apianalytics.SubjectOperatorStateAnalytics, p.cfg.QueueGroup, p.handleOpState); err != nil {
		return fmt.Errorf("analytics/service: subscribe %q: %w", apianalytics.SubjectOperatorStateAnalytics, err)
	}
	if err := p.bus.Subscribe(ctx, apianalytics.SubjectRecordingUploadedWildcard, p.cfg.QueueGroup, p.handleRecUploaded); err != nil {
		return fmt.Errorf("analytics/service: subscribe %q: %w", apianalytics.SubjectRecordingUploadedWildcard, err)
	}

	p.logger.Info("analytics ingest pipeline started",
		zap.Int("batch_size", p.cfg.BatchSize),
		zap.Duration("flush_interval", p.cfg.FlushInterval),
		zap.Int("dedup_size", p.cfg.DedupSize),
		zap.String("queue_group", p.cfg.QueueGroup),
	)

	// Ticker goroutine: force-flush every FlushInterval. Exits on
	// ctx.Done. A WaitGroup blocks Run's return until the ticker has
	// fully exited so goleak sees no straggler.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(p.cfg.FlushInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				p.flushAll(ctx)
			}
		}
	}()

	<-ctx.Done()
	wg.Wait()

	// Drain phase: detach the cancellation signal so the final flushes
	// have time to complete, but keep ctx values (trace/log
	// correlation) via context.WithoutCancel. WithTimeout adds the
	// drain deadline; no contextcheck suppression needed.
	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.DrainTimeout)
	defer drainCancel()
	p.flushAll(drainCtx)

	p.logger.Info("analytics ingest pipeline stopped", zap.Error(ctx.Err()))
	return ctx.Err()
}

// handleCalls is invoked by the bus push consumer on every
// analytics.event.calls message. Returns nil → bus Acks; returns err →
// bus NakWithDelay schedules redelivery.
//
// Decision tree:
//   - json.Unmarshal failure → ack + IncDeadLetter (redelivery of
//     poison would loop).
//   - EventID == uuid.Nil → ack + IncDeadLetter (sentinel: producer
//     forgot to set it; dedup LRU requires non-zero key).
//   - DedupLRU dup hit → ack + IncDedupHit (already inserted; skip).
//   - newly-inserted into LRU → append to buffer; flush if full.
func (p *IngestPipeline) handleCalls(subject string, payload []byte) error {
	p.metrics.IncReceived(subject)

	var row apianalytics.AnalyticsCallEventPayload
	if err := json.Unmarshal(payload, &row); err != nil {
		p.logger.Debug("analytics ingest: malformed calls payload — dead-letter",
			zap.String("subject", subject),
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		p.metrics.IncDeadLetter(subject)
		return nil // ack: redelivery would loop on poison
	}
	if row.EventID == uuid.Nil {
		p.logger.Debug("analytics ingest: calls payload missing event_id — dead-letter",
			zap.String("subject", subject),
		)
		p.metrics.IncDeadLetter(subject)
		return nil
	}

	if dup := p.callsLRU.Add(row.EventID); dup {
		p.metrics.IncDedupHit(subject)
		return nil
	}

	p.mu.Lock()
	p.callsBuf = append(p.callsBuf, row)
	full := len(p.callsBuf) >= p.cfg.BatchSize
	p.mu.Unlock()

	if full {
		// Count-threshold flush runs inside the bus handler, which
		// carries no ctx. WithoutCancel propagates Run's ctx values
		// (trace/log correlation) while detaching from cancellation —
		// an in-flight flush completes even if the parent ctx fires.
		p.flushCalls(context.WithoutCancel(p.runCtx))
	}
	return nil
}

// handleOpState is the operator_state analogue of handleCalls.
// ProjectID is *uuid.UUID (Nullable in CH) — nil is allowed and round-
// trips through json correctly via the *uuid.UUID JSON tag.
func (p *IngestPipeline) handleOpState(subject string, payload []byte) error {
	p.metrics.IncReceived(subject)

	var row apianalytics.AnalyticsOperatorStateEventPayload
	if err := json.Unmarshal(payload, &row); err != nil {
		p.logger.Debug("analytics ingest: malformed operator_state payload — dead-letter",
			zap.String("subject", subject),
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		p.metrics.IncDeadLetter(subject)
		return nil
	}
	if row.EventID == uuid.Nil {
		p.logger.Debug("analytics ingest: operator_state payload missing event_id — dead-letter",
			zap.String("subject", subject),
		)
		p.metrics.IncDeadLetter(subject)
		return nil
	}

	if dup := p.opStateLRU.Add(row.EventID); dup {
		p.metrics.IncDedupHit(subject)
		return nil
	}

	p.mu.Lock()
	p.opStateBuf = append(p.opStateBuf, row)
	full := len(p.opStateBuf) >= p.cfg.BatchSize
	p.mu.Unlock()

	if full {
		p.flushOpState(context.WithoutCancel(p.runCtx))
	}
	return nil
}

// handleRecUploaded is the per-tenant recording.uploaded analogue.
//
// Plan 13.2 § Q4 says the tenant_id is in the payload — NOT extracted
// from the subject token. The subject extraction is only useful as a
// sanity check (TenantID in payload must match subject token 2; a
// mismatch is suspicious but not fatal — log at debug + ack + dead-letter).
// v1 trusts the payload.
func (p *IngestPipeline) handleRecUploaded(subject string, payload []byte) error {
	p.metrics.IncReceived(subject)

	var row recordingapi.RecordingUploadedEvent
	if err := json.Unmarshal(payload, &row); err != nil {
		p.logger.Debug("analytics ingest: malformed recording.uploaded payload — dead-letter",
			zap.String("subject", subject),
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		p.metrics.IncDeadLetter(subject)
		return nil
	}
	if row.EventID == uuid.Nil {
		p.logger.Debug("analytics ingest: recording.uploaded payload missing event_id — dead-letter",
			zap.String("subject", subject),
		)
		p.metrics.IncDeadLetter(subject)
		return nil
	}

	if dup := p.recUploadedLRU.Add(row.EventID); dup {
		p.metrics.IncDedupHit(subject)
		return nil
	}

	p.mu.Lock()
	p.recUploadedBuf = append(p.recUploadedBuf, row)
	full := len(p.recUploadedBuf) >= p.cfg.BatchSize
	p.mu.Unlock()

	if full {
		p.flushRecUploaded(context.WithoutCancel(p.runCtx))
	}
	return nil
}

// flushAll forces a flush of every non-empty buffer. Called by the
// ticker and by the drain path on ctx.Done. The store helpers tolerate
// empty slices, but we skip empty buffers anyway to avoid noise in the
// batch_size histogram.
func (p *IngestPipeline) flushAll(ctx context.Context) {
	p.flushCalls(ctx)
	p.flushOpState(ctx)
	p.flushRecUploaded(ctx)
}

// flushCalls drains the calls buffer into the store under ctx. On
// failure, the rows are LOST (their event_ids are already in the LRU,
// so re-enqueueing would dedup); we tick IncFailed and log at error.
// This trade-off is documented in Plan 13.2 § Step 3.4.
//
// The buffer is swapped under the mutex BEFORE the store call — this
// keeps the lock-held window small (microseconds) and lets a slow CH
// flush concurrently with new handler appends.
func (p *IngestPipeline) flushCalls(ctx context.Context) {
	p.mu.Lock()
	if len(p.callsBuf) == 0 {
		p.mu.Unlock()
		return
	}
	rows := p.callsBuf
	p.callsBuf = make([]apianalytics.AnalyticsCallEventPayload, 0, p.cfg.BatchSize)
	p.mu.Unlock()

	subject := apianalytics.SubjectCallsAnalytics
	p.metrics.ObserveBatchSize(subject, len(rows))
	start := time.Now()
	err := p.store.InsertCalls(ctx, rows)
	p.metrics.ObserveFlushLatency(subject, time.Since(start).Seconds())
	if err != nil {
		p.logger.Error("analytics ingest: flush calls failed",
			zap.Int("rows", len(rows)),
			zap.Error(err),
		)
		p.metrics.IncFailed(subject, classifyStoreErr(err))
		return
	}
	p.metrics.IncInserted(subject, len(rows))
}

// flushOpState — operator_state analogue of flushCalls.
func (p *IngestPipeline) flushOpState(ctx context.Context) {
	p.mu.Lock()
	if len(p.opStateBuf) == 0 {
		p.mu.Unlock()
		return
	}
	rows := p.opStateBuf
	p.opStateBuf = make([]apianalytics.AnalyticsOperatorStateEventPayload, 0, p.cfg.BatchSize)
	p.mu.Unlock()

	subject := apianalytics.SubjectOperatorStateAnalytics
	p.metrics.ObserveBatchSize(subject, len(rows))
	start := time.Now()
	err := p.store.InsertOperatorStates(ctx, rows)
	p.metrics.ObserveFlushLatency(subject, time.Since(start).Seconds())
	if err != nil {
		p.logger.Error("analytics ingest: flush operator_state failed",
			zap.Int("rows", len(rows)),
			zap.Error(err),
		)
		p.metrics.IncFailed(subject, classifyStoreErr(err))
		return
	}
	p.metrics.IncInserted(subject, len(rows))
}

// flushRecUploaded — recording.uploaded analogue of flushCalls.
func (p *IngestPipeline) flushRecUploaded(ctx context.Context) {
	p.mu.Lock()
	if len(p.recUploadedBuf) == 0 {
		p.mu.Unlock()
		return
	}
	rows := p.recUploadedBuf
	p.recUploadedBuf = make([]recordingapi.RecordingUploadedEvent, 0, p.cfg.BatchSize)
	p.mu.Unlock()

	subject := apianalytics.SubjectRecordingUploadedWildcard
	p.metrics.ObserveBatchSize(subject, len(rows))
	start := time.Now()
	err := p.store.InsertRecordingsUploaded(ctx, rows)
	p.metrics.ObserveFlushLatency(subject, time.Since(start).Seconds())
	if err != nil {
		p.logger.Error("analytics ingest: flush recording.uploaded failed",
			zap.Int("rows", len(rows)),
			zap.Error(err),
		)
		p.metrics.IncFailed(subject, classifyStoreErr(err))
		return
	}
	p.metrics.IncInserted(subject, len(rows))
}

// classifyStoreErr maps a store-layer error to a bounded reason label
// for IncFailed. The string set is the documented enum on the metric
// (prepare_batch | send | other) so cardinality stays small.
//
// We don't unwrap a typed error from clickhouse-go here — the
// distinction matters only for dashboards. The match anchors include
// the "analytics/store:" prefix from batch.go's error wrapping so an
// inner driver message like "tcp send buffer full" does NOT
// false-positive into the "send" bucket.
func classifyStoreErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "analytics/store: prepare batch"):
		return "prepare_batch"
	case strings.Contains(msg, "analytics/store: send"):
		return "send"
	default:
		return "other"
	}
}

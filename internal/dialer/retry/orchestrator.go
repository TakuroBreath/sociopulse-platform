package retry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// Default sweep parameters. Tunable via Config; the defaults match the
// Plan 10 §8.5 cadence (30s sweep, 100-row batch, 3-attempt cap).
const (
	defaultInterval    = 30 * time.Second
	defaultBatchLimit  = 100
	defaultMaxAttempts = DefaultMaxAttempts
)

// Decryptor reverses the per-tenant phone encryption applied by the
// crm RespondentService at insert time. The orchestrator depends on
// this small interface (not directly on tenancy.KMSResolver) for two
// reasons:
//
//  1. Test wiring substitutes a fake without dragging the KMS module in.
//  2. Future encryption changes (e.g. a per-row DEK schema) can swap
//     the implementation without touching this package.
//
// Production wiring passes a thin adapter around tenancy.KMSResolver
// (the same surface the crm service uses on the read path).
type Decryptor interface {
	// Decrypt resolves the per-tenant DEK and decrypts the supplied
	// ciphertext. The returned bytes are the E.164 phone number used
	// by the dialer.
	Decrypt(ctx context.Context, tenantID uuid.UUID, ciphertext []byte) ([]byte, error)
}

// Config bundles the dependencies and settings for an Orchestrator.
// Required fields are documented per-field; nil-tolerated fields fall
// back to safe defaults so the constructor stays trivially wireable
// from tests.
type Config struct {
	// Leader is the leader-election primitive. Required. Production
	// wiring passes *PgLeader; tests pass an in-memory fake satisfying
	// the Leader interface.
	Leader Leader

	// Reader is the Postgres surface — listing mature retries, marking
	// rows exhausted / scheduled. Required.
	Reader RespondentReader

	// Decryptor decrypts the row's phone_encrypted before enqueueing.
	// Required: the dialer worker dials in plaintext, so the queue
	// payload must carry a plaintext E.164.
	Decryptor Decryptor

	// Queue is the dialer CallQueue. Required.
	Queue api.CallQueue

	// LockKey is the advisory-lock key (informational only — Leader
	// holds the actual lock). Stored on the orchestrator for
	// diagnostics / metrics labelling. 0 → DefaultLockKey.
	LockKey int64

	// Interval is the sweep tick cadence. 0 → defaultInterval (30s).
	Interval time.Duration

	// BatchLimit is the per-tick row cap. 0 → defaultBatchLimit (100).
	BatchLimit int

	// MaxAttempts is the per-respondent retry cap. 0 → DefaultMaxAttempts (3).
	MaxAttempts int

	// Logger receives per-method diagnostics. nil → zap.NewNop(). Per
	// Plan 09 carry-forward, fields are typed (zap.String / zap.Stringer)
	// and never carry PII (phone, operator name).
	Logger *zap.Logger

	// Clock returns the current time. nil → time.Now. Tests pass a
	// frozen clock so sweep timestamps are deterministic.
	Clock func() time.Time

	// Metrics is the per-package collector group. nil → no metrics
	// (the Orchestrator is fully functional without it).
	Metrics *Metrics
}

// Leader is the small surface this package consumes from PgLeader.
// Declared as an interface so unit tests can substitute a fake without
// pulling Postgres in.
type Leader interface {
	// Acquire attempts to take leadership; ok=true when held by this
	// instance, ok=false when held by a peer.
	Acquire(ctx context.Context) (bool, error)
	// Release relinquishes leadership; idempotent on a non-leading
	// instance.
	Release(ctx context.Context)
	// IsLeading reports whether this instance currently leads.
	IsLeading() bool
}

// Orchestrator implements api.RetryOrchestrator. The Run loop ticks on
// the configured interval, attempts to acquire the advisory lock, and
// (when leading) sweeps mature respondents into the dialer queue.
type Orchestrator struct {
	leader   Leader
	reader   RespondentReader
	decrypt  Decryptor
	queue    api.CallQueue
	lockKey  int64
	interval time.Duration
	batch    int
	maxAtt   int
	log      *zap.Logger
	clock    func() time.Time
	metrics  *Metrics
}

// Compile-time interface check. Surfaces api.RetryOrchestrator
// signature drift the moment it happens (per Plan 09 lessons #8).
var _ api.RetryOrchestrator = (*Orchestrator)(nil)

// New constructs an Orchestrator. Returns an error when a required
// dependency is missing; nil-tolerated fields are filled with defaults.
func New(cfg Config) (*Orchestrator, error) {
	if cfg.Leader == nil {
		return nil, errors.New("retry.New: Leader is required")
	}
	if cfg.Reader == nil {
		return nil, errors.New("retry.New: Reader is required")
	}
	if cfg.Decryptor == nil {
		return nil, errors.New("retry.New: Decryptor is required")
	}
	if cfg.Queue == nil {
		return nil, errors.New("retry.New: Queue is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	batch := cfg.BatchLimit
	if batch <= 0 {
		batch = defaultBatchLimit
	}
	maxAtt := cfg.MaxAttempts
	if maxAtt <= 0 {
		maxAtt = defaultMaxAttempts
	}
	lockKey := cfg.LockKey
	if lockKey == 0 {
		lockKey = DefaultLockKey
	}
	return &Orchestrator{
		leader:   cfg.Leader,
		reader:   cfg.Reader,
		decrypt:  cfg.Decryptor,
		queue:    cfg.Queue,
		lockKey:  lockKey,
		interval: interval,
		batch:    batch,
		maxAtt:   maxAtt,
		log:      logger,
		clock:    clock,
		metrics:  cfg.Metrics,
	}, nil
}

// Run blocks until ctx cancels. Each tick:
//
//  1. Attempt Leader.Acquire.
//  2. If we lead: run one sweep (PG read → per-row decisions → queue +
//     row updates).
//  3. If we don't lead: log at debug, set leader_active=0, continue.
//
// On ctx cancellation the loop terminates cleanly: any held lock is
// Released so a peer takes over without waiting for TCP keepalive
// timeouts. The shutdown path uses context.Background for the Release
// because the caller's ctx is already cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()

	o.log.Info("retry orchestrator starting",
		zap.Duration("interval", o.interval),
		zap.Int("batch_limit", o.batch),
		zap.Int("max_attempts", o.maxAtt),
		zap.Int64("lock_key", o.lockKey),
	)

	// Run an immediate first sweep on start so the orchestrator
	// doesn't sit idle for a full interval after boot.
	o.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			//nolint:contextcheck // intentional: release lock even when caller ctx is done.
			o.leader.Release(context.Background())
			o.metrics.setLeaderActive(false)
			o.log.Info("retry orchestrator stopped", zap.Error(ctx.Err()))
			return ctx.Err()
		case <-ticker.C:
			o.tick(ctx)
		}
	}
}

// tick is one Run-loop iteration: acquire-or-skip, then sweep.
// Exposed for tests via testTick (see orchestrator_test.go) — production
// callers go through Run.
func (o *Orchestrator) tick(ctx context.Context) {
	leading, err := o.leader.Acquire(ctx)
	if err != nil {
		o.log.Warn("leader acquire failed; skipping sweep",
			zap.Error(err),
		)
		o.metrics.setLeaderActive(false)
		return
	}
	o.metrics.setLeaderActive(leading)
	if !leading {
		// Peer is leader; nothing to do this tick.
		return
	}
	o.sweep(ctx)
}

// sweep runs one batch of mature retries. Reads + per-row decisions are
// timed; the duration histogram covers the whole pipeline so a slow PG
// or a slow Redis surfaces in the same bucket.
func (o *Orchestrator) sweep(ctx context.Context) {
	start := o.clock()
	defer func() {
		o.metrics.observeSweepDuration(o.clock().Sub(start).Seconds())
	}()

	rows, err := o.reader.ListMatureRetries(ctx, o.batch)
	if err != nil {
		o.log.Error("list mature retries failed",
			zap.Error(err),
		)
		return
	}
	if len(rows) == 0 {
		o.log.Debug("no mature retries this sweep")
		return
	}

	o.log.Debug("sweep batch", zap.Int("rows", len(rows)))
	for _, row := range rows {
		o.handleRow(ctx, row)
	}
}

// handleRow processes one mature respondent: exhaust if over the cap,
// otherwise decrypt + enqueue + mark scheduled. Any per-row error is
// logged + bucketed under metric label "skip" so a single bad row
// doesn't poison the rest of the sweep.
func (o *Orchestrator) handleRow(ctx context.Context, row MatureRetryRow) {
	if row.Attempts >= o.maxAtt {
		// Already at or over cap: mark exhausted, never enqueue.
		if err := o.reader.MarkExhausted(ctx, row.ID); err != nil {
			o.log.Error("mark exhausted failed",
				zap.Stringer("respondent_id", row.ID),
				zap.Error(err),
			)
			o.metrics.observeSweep(resultSkip)
			return
		}
		o.metrics.observeSweep(resultExhausted)
		o.log.Info("respondent exhausted",
			zap.Stringer("respondent_id", row.ID),
			zap.Int("attempts", row.Attempts),
		)
		return
	}

	phone, err := o.decrypt.Decrypt(ctx, row.TenantID, row.PhoneCiphertext)
	if err != nil {
		o.log.Error("decrypt phone failed",
			zap.Stringer("respondent_id", row.ID),
			zap.Stringer("tenant_id", row.TenantID),
			zap.Error(err),
		)
		o.metrics.observeSweep(resultSkip)
		return
	}

	priority := uint8(min(1+row.Attempts, 9)) //nolint:gosec // bounded by min()=9.
	//nolint:gosec // attempts is < maxAtt < 256; bounded.
	attemptN := uint8(row.Attempts + 1)
	ok, err := o.queue.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     row.TenantID,
		ProjectID:    row.ProjectID,
		RespondentID: row.ID,
		Phone:        string(phone),
		Region:       row.Region,
		Priority:     priority,
		AttemptN:     attemptN,
	})
	if err != nil {
		o.log.Error("enqueue retry failed",
			zap.Stringer("respondent_id", row.ID),
			zap.Error(err),
		)
		o.metrics.observeSweep(resultSkip)
		return
	}
	if !ok {
		// Already in queue (the previous tick enqueued and our
		// MarkScheduled didn't run, or a manual operator path enqueued
		// out of band). Skip the row but still flip its DB state so we
		// don't re-pick next sweep.
		o.log.Debug("respondent already in queue; skipping enqueue",
			zap.Stringer("respondent_id", row.ID),
		)
	}

	if err := o.reader.MarkScheduled(ctx, row.ID); err != nil {
		o.log.Error("mark scheduled failed",
			zap.Stringer("respondent_id", row.ID),
			zap.Error(err),
		)
		o.metrics.observeSweep(resultSkip)
		return
	}
	o.metrics.observeSweep(resultEnqueued)
}

// String implements fmt.Stringer — handy for log fields when a
// composer logs "started %s". Not on the api.RetryOrchestrator surface.
func (o *Orchestrator) String() string {
	return fmt.Sprintf("retry.Orchestrator{interval=%s, batch=%d, max_attempts=%d, lock_key=%d}",
		o.interval, o.batch, o.maxAtt, o.lockKey)
}

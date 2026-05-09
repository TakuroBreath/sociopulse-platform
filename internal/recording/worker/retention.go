package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Default sweep parameters. Tunable via RetentionConfig; the defaults
// match Plan 12.4 §8.5: 5min sweep cadence, 100-row batch.
const (
	defaultRetentionInterval = 5 * time.Minute
	defaultRetentionBatch    = 100
)

// passLabelColdMove + passLabelDelete + actionLabel* are the bounded-
// cardinality label values for the metrics collectors. Constants keep
// the call-sites typo-resistant.
const (
	passLabelColdMove = "cold_move"
	passLabelDelete   = "delete"

	// actionLabel* are past-participle to match the audit-action constants
	// (rapi.AuditActionColdMoved / rapi.AuditActionDeleted) and the plan
	// spec. Operations dashboards joining audit_log.action and the metric
	// `action` label benefit from consistent verb tense.
	actionLabelColdMove = "cold_moved"
	actionLabelDelete   = "deleted"

	resultLabelOK       = "ok"
	resultLabelStale    = "stale"
	resultLabelError    = "error"
	resultLabelOrphaned = "orphaned"

	deleteReasonRetention = "retention"
)

// errStaleSkip is the in-Tx sentinel returned when MarkColdTx /
// MarkDeletedTx report rowsAffected=0 — a benign concurrent-flip skip.
// Caller treats this as a non-error "stale" outcome rather than a hard
// failure. Defined as an unexported sentinel so the production callers
// in this package can errors.Is against it without exposing the
// concept across the package boundary.
var errStaleSkip = errors.New("recording.worker: stale row (concurrent state change)")

// Leader is the small surface this worker consumes from a leader-
// election primitive. Production wiring passes *retry.PgLeader (which
// satisfies this interface); tests pass an in-memory fake. The type
// is a worker-package type because the worker is the only consumer —
// declaring it here keeps the dependency arrow pointing inward
// (worker → retry → postgres, not retry ← worker).
type Leader interface {
	// Acquire attempts to take leadership; ok=true when held by this
	// instance, ok=false when held by a peer.
	Acquire(ctx context.Context) (bool, error)
	// Release relinquishes leadership; idempotent on a non-leading
	// instance.
	Release(ctx context.Context)
	// Key returns the advisory-lock key. Used for diagnostic logging.
	Key() int64
}

// RetentionConfig wires the dependencies and tunables for a
// RetentionPass. Required fields are validated by NewRetentionPass;
// nil-tolerant fields fall back to safe defaults.
type RetentionConfig struct {
	// Pool is the Postgres pool used for the per-row WithTenant
	// transactions that flip status + write audit + (delete only)
	// append the outbox row. Required.
	Pool *postgres.Pool

	// Leader is the leader-election primitive. Required. Production
	// wiring passes *retry.PgLeader constructed against
	// RetentionLockKey.
	Leader Leader

	// Store is the LifecycleStore — workers list across tenants and
	// flip the per-row status via MarkColdTx / MarkDeletedTx.
	// Required.
	Store LifecycleStore

	// Objects is the object-storage backend. Required for Phase A
	// of the hard-delete path (purging the audio object before the
	// status flip). Cold-move never calls Objects.
	Objects storage.ObjectStore

	// Outbox is the transactional-outbox writer. Required for the
	// hard-delete path: the recording.call.deleted event commits in
	// the same Tx as the audit row + status flip.
	Outbox outbox.Writer

	// Metrics receives per-pass + per-row observations. nil →
	// metrics-disabled (the worker is fully functional without).
	Metrics *metrics.RecordingMetrics

	// Logger receives per-method diagnostics. nil → zap.NewNop.
	// Per Plan 09 carry-forward, fields are typed (zap.String /
	// zap.Stringer) and never carry PII.
	Logger *zap.Logger

	// Interval is the Run-loop tick cadence. 0 → defaultRetentionInterval
	// (5 minutes).
	Interval time.Duration

	// Batch is the per-pass row cap (passed to ListDueColdMoves +
	// ListDueDeletes). 0 → defaultRetentionBatch (100).
	Batch int
}

// RetentionPass is the leader-elected daemon that runs the recording
// retention sweeps once per ticker tick. Each tick:
//
//  1. Acquire-or-skip the advisory lock.
//  2. If leading: run sweepColdMoves THEN sweepDeletes (each a
//     LIST → per-row Tx batch).
//  3. On ctx cancel: release the lock with a detached background ctx
//     so peers take over without waiting for TCP keepalive.
//
// Mirrors internal/dialer/retry/orchestrator.go's shape.
type RetentionPass struct {
	pool     *postgres.Pool
	leader   Leader
	store    LifecycleStore
	objects  storage.ObjectStore
	outbox   outbox.Writer
	metrics  *metrics.RecordingMetrics
	log      *zap.Logger
	interval time.Duration
	batch    int
}

// NewRetentionPass constructs a RetentionPass. Returns an error when a
// required dependency is missing; nil-tolerant fields are filled with
// defaults.
func NewRetentionPass(cfg RetentionConfig) (*RetentionPass, error) {
	if cfg.Pool == nil {
		return nil, errors.New("worker.NewRetentionPass: Pool is required")
	}
	if cfg.Leader == nil {
		return nil, errors.New("worker.NewRetentionPass: Leader is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("worker.NewRetentionPass: Store is required")
	}
	if cfg.Objects == nil {
		return nil, errors.New("worker.NewRetentionPass: Objects is required")
	}
	if cfg.Outbox == nil {
		return nil, errors.New("worker.NewRetentionPass: Outbox is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultRetentionInterval
	}
	batch := cfg.Batch
	if batch <= 0 {
		batch = defaultRetentionBatch
	}
	return &RetentionPass{
		pool:     cfg.Pool,
		leader:   cfg.Leader,
		store:    cfg.Store,
		objects:  cfg.Objects,
		outbox:   cfg.Outbox,
		metrics:  cfg.Metrics,
		log:      logger,
		interval: interval,
		batch:    batch,
	}, nil
}

// Run blocks until ctx cancels. Each tick: leader-acquire-or-skip +
// SweepOnce. On ctx cancellation the loop terminates cleanly — any
// held lock is Released so a peer takes over without waiting for TCP
// keepalive timeouts.
func (p *RetentionPass) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	p.log.Info("recording retention pass starting",
		zap.Duration("interval", p.interval),
		zap.Int("batch", p.batch),
		zap.Int64("lock_key", p.leader.Key()),
	)

	// Run an immediate first sweep on start so the worker doesn't sit
	// idle for a full interval after boot.
	p.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			//nolint:contextcheck // intentional: release lock even when caller ctx is done.
			p.leader.Release(context.Background())
			p.log.Info("recording retention pass stopped", zap.Error(ctx.Err()))
			return ctx.Err()
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

// tick is one Run-loop iteration: acquire-or-skip, then SweepOnce.
// Per-sweep errors are logged + swallowed so a single bad tick doesn't
// poison the loop.
func (p *RetentionPass) tick(ctx context.Context) {
	leading, err := p.leader.Acquire(ctx)
	if err != nil {
		p.log.Warn("retention leader acquire failed; skipping tick",
			zap.Error(err),
		)
		return
	}
	if !leading {
		p.log.Debug("retention leader held by peer; skipping tick")
		return
	}
	if sweepErr := p.SweepOnce(ctx); sweepErr != nil {
		p.log.Error("retention sweep failed",
			zap.Error(sweepErr),
		)
	}
}

// SweepOnce runs cold-moves THEN deletes in sequence. Public test seam
// — production callers go through Run.
//
// Per-row failures inside each pass are isolated (logged + bumped on
// the metric, sweep continues). SweepOnce only returns an error when
// one of the LIST queries itself fails — which is rare and indicates
// a Postgres-level outage worth surfacing.
func (p *RetentionPass) SweepOnce(ctx context.Context) error {
	if err := p.sweepColdMoves(ctx); err != nil {
		return fmt.Errorf("retention.cold_moves: %w", err)
	}
	if err := p.sweepDeletes(ctx); err != nil {
		return fmt.Errorf("retention.deletes: %w", err)
	}
	return nil
}

// sweepColdMoves lists due cold-move rows and applies each one.
func (p *RetentionPass) sweepColdMoves(ctx context.Context) error {
	start := time.Now()
	now := start.UTC()

	rows, err := p.store.ListDueColdMoves(ctx, now, p.batch)
	if err != nil {
		p.metrics.ObserveRetentionPass(passLabelColdMove, resultLabelError, time.Since(start).Seconds())
		return fmt.Errorf("list due cold moves: %w", err)
	}
	if len(rows) == 0 {
		p.metrics.ObserveRetentionPass(passLabelColdMove, resultLabelOK, time.Since(start).Seconds())
		p.log.Debug("retention cold-move sweep: no rows")
		return nil
	}

	p.log.Debug("retention cold-move sweep", zap.Int("rows", len(rows)))
	for _, row := range rows {
		p.handleColdMove(ctx, row, now)
	}
	p.metrics.ObserveRetentionPass(passLabelColdMove, resultLabelOK, time.Since(start).Seconds())
	return nil
}

// handleColdMove applies one cold-move: MarkColdTx + audit row inside
// a single WithTenant Tx. errStaleSkip → metric "stale" + return. Any
// other Tx error → metric "error" + log warn + return; the next sweep
// will pick the row up again.
func (p *RetentionPass) handleColdMove(ctx context.Context, row rapi.LifecycleRow, now time.Time) {
	err := p.pool.WithTenant(ctx, row.TenantID, func(tx postgres.Tx) error {
		n, mErr := p.store.MarkColdTx(ctx, tx, row.ID)
		if mErr != nil {
			return fmt.Errorf("mark cold: %w", mErr)
		}
		if n == 0 {
			return errStaleSkip
		}
		payload := map[string]any{
			"recording_id":     row.ID,
			"call_id":          row.CallID,
			"audio_object_key": row.AudioObjectKey,
			"cold_at":          row.ColdAt,
			"prev_status":      row.Status,
		}
		if aErr := writeAudit(ctx, tx, row.TenantID, row.ID, rapi.AuditActionColdMoved, payload, now); aErr != nil {
			return fmt.Errorf("audit: %w", aErr)
		}
		return nil
	})

	tenantLabel := row.TenantID.String()
	switch {
	case errors.Is(err, errStaleSkip):
		p.metrics.IncRetentionAction(tenantLabel, actionLabelColdMove, resultLabelStale)
		p.log.Debug("retention cold-move stale (concurrent flip)",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
		)
	case err != nil:
		p.metrics.IncRetentionAction(tenantLabel, actionLabelColdMove, resultLabelError)
		p.log.Warn("retention cold-move failed",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
			zap.Error(err),
		)
	default:
		p.metrics.IncRetentionAction(tenantLabel, actionLabelColdMove, resultLabelOK)
		p.log.Info("retention cold-move applied",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
		)
	}
}

// sweepDeletes lists due-delete rows and applies each one.
func (p *RetentionPass) sweepDeletes(ctx context.Context) error {
	start := time.Now()
	now := start.UTC()

	rows, err := p.store.ListDueDeletes(ctx, now, p.batch)
	if err != nil {
		p.metrics.ObserveRetentionPass(passLabelDelete, resultLabelError, time.Since(start).Seconds())
		return fmt.Errorf("list due deletes: %w", err)
	}
	if len(rows) == 0 {
		p.metrics.ObserveRetentionPass(passLabelDelete, resultLabelOK, time.Since(start).Seconds())
		p.log.Debug("retention delete sweep: no rows")
		return nil
	}

	p.log.Debug("retention delete sweep", zap.Int("rows", len(rows)))
	for _, row := range rows {
		p.handleDelete(ctx, row, now)
	}
	p.metrics.ObserveRetentionPass(passLabelDelete, resultLabelOK, time.Since(start).Seconds())
	return nil
}

// handleDelete is the two-phase hard-delete:
//
//  1. Phase A: ObjectStore.Delete (irreversible). On ErrObjectNotFound
//     the audio is already gone — log + bump "orphaned" + still proceed
//     to Phase B so DB and S3 reconcile. On generic error: log warn +
//     bump "error" + return WITHOUT Phase B (the next sweep retries).
//
//  2. Phase B: pool.WithTenant Tx running MarkDeletedTx + audit +
//     outbox.Append. errStaleSkip → metric "stale" + return. Any other
//     Tx error → metric "error". Success → metric "ok" (or "orphaned"
//     if Phase A reported the object was already gone).
func (p *RetentionPass) handleDelete(ctx context.Context, row rapi.LifecycleRow, now time.Time) {
	tenantLabel := row.TenantID.String()

	// Phase A: irreversible audio-object delete.
	orphaned := false
	if delErr := p.objects.Delete(ctx, row.S3Bucket, row.AudioObjectKey); delErr != nil {
		switch {
		case errors.Is(delErr, storage.ErrObjectNotFound):
			orphaned = true
			p.log.Debug("retention delete Phase A: object already gone (orphaned)",
				zap.String("recording_id", row.ID.String()),
				zap.String("tenant_id", tenantLabel),
				zap.String("bucket", row.S3Bucket),
				zap.String("key", row.AudioObjectKey),
			)
			// Fall through to Phase B — DB still needs reconciliation.
		default:
			p.metrics.IncRetentionAction(tenantLabel, actionLabelDelete, resultLabelError)
			p.log.Warn("retention delete Phase A failed; will retry next sweep",
				zap.String("recording_id", row.ID.String()),
				zap.String("tenant_id", tenantLabel),
				zap.Error(delErr),
			)
			return
		}
	}

	// Phase B: atomic DB state change.
	deletedAt := now
	err := p.pool.WithTenant(ctx, row.TenantID, func(tx postgres.Tx) error {
		n, mErr := p.store.MarkDeletedTx(ctx, tx, row.ID)
		if mErr != nil {
			return fmt.Errorf("mark deleted: %w", mErr)
		}
		if n == 0 {
			return errStaleSkip
		}
		payload := map[string]any{
			"recording_id":     row.ID,
			"call_id":          row.CallID,
			"audio_object_key": row.AudioObjectKey,
			"prev_status":      row.Status,
			"orphaned":         orphaned,
			"reason":           deleteReasonRetention,
		}
		if aErr := writeAudit(ctx, tx, row.TenantID, row.ID, rapi.AuditActionDeleted, payload, deletedAt); aErr != nil {
			return fmt.Errorf("audit: %w", aErr)
		}
		if oErr := p.appendDeletedOutbox(ctx, tx, row, deletedAt); oErr != nil {
			return fmt.Errorf("outbox: %w", oErr)
		}
		return nil
	})

	switch {
	case errors.Is(err, errStaleSkip):
		p.metrics.IncRetentionAction(tenantLabel, actionLabelDelete, resultLabelStale)
		p.log.Debug("retention delete stale (concurrent flip)",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
		)
	case err != nil:
		p.metrics.IncRetentionAction(tenantLabel, actionLabelDelete, resultLabelError)
		p.log.Warn("retention delete Phase B failed",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
			zap.Error(err),
		)
	case orphaned:
		p.metrics.IncRetentionAction(tenantLabel, actionLabelDelete, resultLabelOrphaned)
		p.log.Info("retention delete reconciled (orphaned audio)",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
		)
	default:
		p.metrics.IncRetentionAction(tenantLabel, actionLabelDelete, resultLabelOK)
		p.log.Info("retention delete applied",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
		)
	}
}

// appendDeletedOutbox builds the RecordingCallDeletedEvent payload and
// appends it via the outbox writer. The writer's Append signature
// REQUIRES a Tx — keeping the outbox row's lifetime atomic with the
// status flip + audit row in the caller's Tx.
func (p *RetentionPass) appendDeletedOutbox(
	ctx context.Context,
	tx postgres.Tx,
	row rapi.LifecycleRow,
	deletedAt time.Time,
) error {
	body, err := json.Marshal(rapi.RecordingCallDeletedEvent{
		RecordingID: row.ID,
		CallID:      row.CallID,
		TenantID:    row.TenantID,
		DeletedAt:   deletedAt.Unix(),
		Reason:      deleteReasonRetention,
	})
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	tenantID := row.TenantID
	callID := row.CallID
	return p.outbox.Append(ctx, tx, outbox.Event{
		TenantID:    &tenantID,
		AggregateID: &callID,
		Subject:     rapi.SubjectRecordingCallDeletedFor(row.TenantID),
		Payload:     body,
	})
}

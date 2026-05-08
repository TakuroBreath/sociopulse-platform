package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	"github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// purgeBypassRunner is the slice of *postgres.Pool the PurgeWorker
// uses. The worker runs cross-tenant by design (the daily purge
// cron task does not know the per-row tenant up front), so it
// requires BypassRLS rather than WithTenant. *postgres.Pool
// satisfies this interface; tests substitute an in-memory fake.
type purgeBypassRunner interface {
	BypassRLS(ctx context.Context, fn func(postgres.Tx) error) error
}

// defaultPurgeBatch is the number of rows the worker hard-deletes per
// tick. 1000 keeps the delete tx short (so RLS-bypass holds the
// connection only briefly) and lets the cron run once per day with
// plenty of headroom for a tenant that accumulates 30k+ deletions
// in a single day.
const defaultPurgeBatch = 1000

// PurgeWorker is the asynq.Handler-shaped entry point for the daily
// "crm:respondents.purge" task. It calls
// RespondentStorePort.PurgeOlderThan inside a BypassRLS transaction
// (so cross-tenant rows are removed in one pass), then emits one
// audit row per purged id so the 152-ФЗ compliance trail is
// complete.
//
// The worker is intentionally idempotent: re-running with the same
// cutoff is safe (PurgeOlderThan's CTE-based DELETE returns the rows
// it actually removed, so the second pass is a no-op) and
// recoverable — asynq retries on infrastructural failure.
type PurgeWorker struct {
	tx    purgeBypassRunner
	store api.RespondentStorePort
	audit auditapi.Logger

	grace time.Duration
	batch int
	clock func() time.Time

	logger *zap.Logger
}

// NewPurgeWorker constructs a PurgeWorker.
//
// pool, store, and auditLogger are mandatory — silently dropping any
// of them would either bypass the audit trail or fail to clean up
// rows we promised the data-subject we would. Misconfigured
// composition roots fail loudly here, not at first invocation.
//
// grace=0 falls back to the canonical 30-day window
// (deletionGracePeriod). batch=0 falls back to defaultPurgeBatch.
// clock=nil falls back to time.Now.
func NewPurgeWorker(
	pool purgeBypassRunner,
	store api.RespondentStorePort,
	auditLogger auditapi.Logger,
	grace time.Duration,
	batch int,
	clock func() time.Time,
) *PurgeWorker {
	if pool == nil {
		panic("crm/service: NewPurgeWorker: pool is required")
	}
	if store == nil {
		panic("crm/service: NewPurgeWorker: store is required")
	}
	if auditLogger == nil {
		panic("crm/service: NewPurgeWorker: auditLogger is required (use a no-op fake in tests, never nil)")
	}
	if grace <= 0 {
		grace = deletionGracePeriod
	}
	if batch <= 0 {
		batch = defaultPurgeBatch
	}
	if clock == nil {
		clock = time.Now
	}
	return &PurgeWorker{
		tx:     pool,
		store:  store,
		audit:  auditLogger,
		grace:  grace,
		batch:  batch,
		clock:  clock,
		logger: zap.NewNop(),
	}
}

// WithLogger attaches a zap logger so the worker can emit structured
// progress / failure logs. Optional; nil falls back to zap.NewNop().
func (w *PurgeWorker) WithLogger(log *zap.Logger) *PurgeWorker {
	if log == nil {
		log = zap.NewNop()
	}
	w.logger = log
	return w
}

// HandlePurgeTask is the asynq handler for the daily purge cron. It
// computes the cutoff (now - grace), calls PurgeOlderThan, audits
// each purged id, and returns nil on success / an error on
// infrastructural failure (asynq retries).
//
// The asynq.Task argument is unused — the cron task carries no
// payload — but the handler signature must match asynq.Handler so
// the same function can be registered on a ServeMux for both cron
// and on-demand triggers.
func (w *PurgeWorker) HandlePurgeTask(ctx context.Context, _ *asynq.Task) error {
	return w.Run(ctx)
}

// Run is the cron-agnostic core of the purge worker. Exposed for
// tests + cmd/worker callers that want to trigger a purge pass
// outside of asynq's scheduler.
//
// Audit failure is non-fatal — once the row is hard-deleted we
// cannot un-delete it, and a missing audit row is recoverable from
// the structured log. Store failure (DB down) is fatal so asynq
// retries the task on the next backoff.
func (w *PurgeWorker) Run(ctx context.Context) error {
	cutoff := w.clock().UTC().Add(-w.grace)
	w.logger.Info("respondent purge starting",
		zap.Time("cutoff", cutoff),
		zap.Int("batch", w.batch),
	)

	var purged []uuid.UUID
	err := w.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		ids, perr := w.store.PurgeOlderThan(ctx, tx, cutoff, w.batch)
		if perr != nil {
			return perr
		}
		purged = ids
		return nil
	})
	if err != nil {
		return fmt.Errorf("crm/service: purge worker: %w", err)
	}

	if len(purged) == 0 {
		w.logger.Debug("respondent purge: nothing to do")
		return nil
	}

	for _, id := range purged {
		if aerr := w.writeAudit(ctx, auditapi.Event{
			Action: "crm.respondent.purged",
			Target: "respondent:" + id.String(),
			Payload: map[string]any{
				"respondent_id": id,
				"cutoff":        cutoff,
			},
		}); aerr != nil {
			// Non-fatal: row is already hard-deleted, surfacing the
			// failure as task error would re-run the entire batch on
			// retry and emit duplicate audit rows.
			w.logger.Warn("audit write failed during purge",
				zap.String("respondent_id", id.String()),
				zap.Error(aerr))
		}
	}

	w.logger.Info("respondent purge finished",
		zap.Int("purged", len(purged)),
	)
	return nil
}

// writeAudit fills in the boilerplate fields (timestamp + system
// actor kind) and invokes the audit Logger. Mirrors the local helper
// in the project / respondent services; we don't share via a
// receiver-typed helper because Go methods can't be defined on a
// helper struct without restructuring the existing services.
//
// The purge worker always runs as ActorSystem (no human actor).
func (w *PurgeWorker) writeAudit(ctx context.Context, ev auditapi.Event) error {
	if w.audit == nil {
		return nil
	}
	if ev.ActorKind == "" {
		ev.ActorKind = auditapi.ActorSystem
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = w.clock()
	}
	if err := w.audit.Write(ctx, ev); err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}

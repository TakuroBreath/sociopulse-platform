package worker

// Package-level note on audit emission:
//
// Plan 12.3's RecordingService.VerifyChecksum (internal/recording/service/
// verify.go) does NOT write an audit row — it is a metadata-level
// integrity check, not an access of plaintext audio. The integrity
// worker is therefore the canonical emitter of recording.verified rows;
// without this, chain-of-custody history would lose a verification's
// outcome whenever the weekly sweep runs.
//
// If a future change moves audit emission into VerifyChecksum itself,
// drop the writeAudit call inside handleVerify (and add a regression
// test asserting the row is no longer double-written).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Default sweep parameters. Tunable via IntegrityConfig; the defaults
// match Plan 12.4 §9.4: hourly tick, 10-row batch, 1% BERNOULLI sample.
//
// SamplePercent rate-limit story: with Interval=1h, Batch=10,
// SamplePercent=1.0, over 7 days that's 168 ticks × 10 rows = 1680
// rows verified per week. The eligibility filter
// `verified_at < now() - interval '7 days'` ensures rows are NOT
// re-verified within the same week — natural rate-limit. SamplePercent
// is the BERNOULLI rate, applied BEFORE the WHERE clause and the LIMIT.
const (
	defaultIntegrityInterval      = 1 * time.Hour
	defaultIntegrityBatch         = 10
	defaultIntegritySamplePercent = 1.0
)

// Integrity action result-label constants. Bounded cardinality so
// IntegrityActionsTotal alerts can join on action+result aggregates.
const (
	integrityResultOK       = "ok"
	integrityResultMismatch = "mismatch"
	integrityResultError    = "error"
	// integrityResultStale fires when UpdateVerifyResultTx reports
	// rowsAffected=0 — the row was concurrently deleted (admin or retention
	// worker) between SampleForVerify and the WithTenant Tx. Skip the audit
	// row (would dangle to a non-existent recording target) and bail out.
	integrityResultStale = "stale"
	// integrityResultEmpty marks a sweep where SampleForVerify returned
	// zero rows. Distinct from "ok" so dashboards can tell a hung daemon
	// (no histogram samples at all) apart from a genuinely empty queue
	// (stream of "empty" samples).
	integrityResultEmpty = "empty"
)

// leaderPassIntegrity is the value of the LeaderActive gauge's `pass`
// label for the integrity sweep — daemon name, not result.
const leaderPassIntegrity = "integrity"

// IntegrityConfig wires the dependencies and tunables for an
// IntegrityPass. Required fields are validated by NewIntegrityPass;
// nil-tolerant fields fall back to safe defaults.
type IntegrityConfig struct {
	// Pool is the Postgres pool used for the per-row WithTenant
	// transactions that persist verified_at + integrity_ok and write
	// the recording.verified audit row in one Tx. Required.
	Pool *postgres.Pool

	// Leader is the leader-election primitive. Required. Production
	// wiring passes *retry.PgLeader constructed against
	// IntegrityLockKey (distinct slot from RetentionLockKey).
	Leader Leader

	// Store is the LifecycleStore — workers sample across tenants via
	// SampleForVerify and persist results via UpdateVerifyResultTx.
	// Required.
	Store LifecycleStore

	// Service is the RecordingService — workers call VerifyChecksum
	// per sampled row to recompute its sha256 against the stored
	// ciphertext. Required. The service runs without a tenant scope on
	// the request — the worker passes (row.TenantID, row.CallID) and
	// VerifyChecksum looks up the row through the per-tenant audited
	// path.
	Service rapi.RecordingService

	// Metrics receives per-pass + per-row observations. nil →
	// metrics-disabled (the worker is fully functional without).
	Metrics *metrics.RecordingMetrics

	// Logger receives per-method diagnostics. nil → zap.NewNop.
	// Per Plan 09 carry-forward, fields are typed (zap.String /
	// zap.Stringer) and never carry PII.
	Logger *zap.Logger

	// Interval is the Run-loop tick cadence. 0 → defaultIntegrityInterval
	// (1 hour, matching Plan 12.4 §9.4).
	Interval time.Duration

	// Batch is the per-pass row cap (passed to SampleForVerify's LIMIT).
	// 0 → defaultIntegrityBatch (10).
	Batch int

	// SamplePercent is the BERNOULLI sample rate (0, 100]. 0 →
	// defaultIntegritySamplePercent (1.0); the store clamps the floor
	// to 0.01 and the ceiling to 100.
	SamplePercent float64
}

// IntegrityPass is the leader-elected daemon that runs the recording
// integrity sweep once per ticker tick. Each tick:
//
//  1. Acquire-or-skip the advisory lock.
//  2. If leading: SweepOnce → SampleForVerify → per-row VerifyChecksum
//     → in-Tx UpdateVerifyResultTx + writeAudit (recording.verified).
//  3. On ctx cancel: release the lock with a detached background ctx
//     so peers take over without waiting for TCP keepalive.
//
// Mirrors RetentionPass (retention.go) shape: the two daemons share
// Run/tick/SweepOnce structure but operate on disjoint row sets and
// distinct advisory-lock slots, so they can lead simultaneously.
type IntegrityPass struct {
	pool      *postgres.Pool
	leader    Leader
	store     LifecycleStore
	service   rapi.RecordingService
	metrics   *metrics.RecordingMetrics
	log       *zap.Logger
	interval  time.Duration
	batch     int
	samplePct float64
}

// NewIntegrityPass constructs an IntegrityPass. Returns an error when a
// required dependency is missing; nil-tolerant fields are filled with
// defaults.
func NewIntegrityPass(cfg IntegrityConfig) (*IntegrityPass, error) {
	if cfg.Pool == nil {
		return nil, errors.New("worker.NewIntegrityPass: Pool is required")
	}
	if cfg.Leader == nil {
		return nil, errors.New("worker.NewIntegrityPass: Leader is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("worker.NewIntegrityPass: Store is required")
	}
	if cfg.Service == nil {
		return nil, errors.New("worker.NewIntegrityPass: Service is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultIntegrityInterval
	}
	batch := cfg.Batch
	if batch <= 0 {
		batch = defaultIntegrityBatch
	}
	samplePct := cfg.SamplePercent
	if samplePct <= 0 {
		samplePct = defaultIntegritySamplePercent
	}
	// Range-check at construction so misconfiguration surfaces on
	// cmd/worker boot rather than buried in the SampleForVerify SQL
	// clamp. Postgres TABLESAMPLE BERNOULLI is defined for (0, 100].
	if samplePct < 0.01 || samplePct > 100.0 {
		return nil, fmt.Errorf(
			"worker.NewIntegrityPass: SamplePercent %.4f out of range [0.01, 100.0]",
			samplePct,
		)
	}
	return &IntegrityPass{
		pool:      cfg.Pool,
		leader:    cfg.Leader,
		store:     cfg.Store,
		service:   cfg.Service,
		metrics:   cfg.Metrics,
		log:       logger,
		interval:  interval,
		batch:     batch,
		samplePct: samplePct,
	}, nil
}

// Run blocks until ctx cancels. Each tick: leader-acquire-or-skip +
// SweepOnce. On ctx cancellation the loop terminates cleanly — any held
// lock is Released so a peer takes over without waiting for TCP
// keepalive timeouts.
//
// Call Run once per process; running multiple instances against the
// same lock key is safe (the advisory lock serialises) but wastes
// goroutines and metric churn.
func (p *IntegrityPass) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	p.log.Info("recording integrity pass starting",
		zap.Duration("interval", p.interval),
		zap.Int("batch", p.batch),
		zap.Float64("sample_percent", p.samplePct),
		zap.Int64("lock_key", p.leader.Key()),
	)

	// Run an immediate first sweep on start so the worker doesn't sit
	// idle for a full interval after boot.
	p.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			// Clear the leader gauge before Release so dashboards see
			// the handoff happen in the right order.
			p.metrics.SetLeaderActive(leaderPassIntegrity, false)
			//nolint:contextcheck // intentional: release lock even when caller ctx is done.
			p.leader.Release(context.Background())
			p.log.Info("recording integrity pass stopped", zap.Error(ctx.Err()))
			return ctx.Err()
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

// tick is one Run-loop iteration: acquire-or-skip, then SweepOnce.
// Per-sweep errors are logged + swallowed so a single bad tick doesn't
// poison the loop.
func (p *IntegrityPass) tick(ctx context.Context) {
	leading, err := p.leader.Acquire(ctx)
	if err != nil {
		p.log.Warn("integrity leader acquire failed; skipping tick",
			zap.Error(err),
		)
		p.metrics.SetLeaderActive(leaderPassIntegrity, false)
		return
	}
	p.metrics.SetLeaderActive(leaderPassIntegrity, leading)
	if !leading {
		p.log.Debug("integrity leader held by peer; skipping tick")
		return
	}
	if sweepErr := p.SweepOnce(ctx); sweepErr != nil {
		p.log.Error("integrity sweep failed",
			zap.Error(sweepErr),
		)
	}
}

// SweepOnce runs one verify pass: SampleForVerify → per-row VerifyChecksum
// → in-Tx persist + audit. Public test seam — production callers go
// through Run.
//
// Per-row failures inside the pass are isolated (logged + bumped on the
// IntegrityActionsTotal metric, sweep continues). SweepOnce only returns
// an error when SampleForVerify itself fails — a rare Postgres-level
// outage worth surfacing.
func (p *IntegrityPass) SweepOnce(ctx context.Context) error {
	start := time.Now()

	rows, err := p.store.SampleForVerify(ctx, p.samplePct, p.batch)
	if err != nil {
		p.metrics.ObserveIntegrityPass(integrityResultError, time.Since(start).Seconds())
		return fmt.Errorf("integrity.sample: %w", err)
	}
	if len(rows) == 0 {
		// "empty" — distinct from "ok" so dashboards can tell a hung
		// daemon (no histogram samples at all) apart from a queue that
		// is genuinely caught up (steady stream of "empty" samples).
		p.metrics.ObserveIntegrityPass(integrityResultEmpty, time.Since(start).Seconds())
		p.log.Debug("integrity sweep: no rows")
		return nil
	}

	p.log.Debug("integrity sweep", zap.Int("rows", len(rows)))
	now := start.UTC()
	for _, row := range rows {
		p.handleVerify(ctx, row, now)
	}
	p.metrics.ObserveIntegrityPass(integrityResultOK, time.Since(start).Seconds())
	return nil
}

// handleVerify runs one row's verify pipeline:
//
//  1. Service.VerifyChecksum — on error, bump "error" metric and return
//     (no UpdateVerifyResult, no audit; row stays eligible for retry).
//  2. WithTenant Tx — UpdateVerifyResultTx + writeAudit
//     (recording.verified). On Tx error, bump "error" metric and return.
//  3. Success classification — result.OK=true → "ok"; result.OK=false →
//     "mismatch" + IncIntegrityFailure (master spec §15.5 alert).
func (p *IntegrityPass) handleVerify(ctx context.Context, row rapi.LifecycleRow, now time.Time) {
	tenantLabel := row.TenantID.String()

	result, err := p.service.VerifyChecksum(ctx, row.TenantID, row.CallID)
	if err != nil {
		p.metrics.IncIntegrityAction(tenantLabel, integrityResultError)
		p.log.Warn("integrity verify failed; will retry next sweep",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
			zap.Error(err),
		)
		return
	}

	persistErr := p.pool.WithTenant(ctx, row.TenantID, func(tx postgres.Tx) error {
		n, uErr := p.store.UpdateVerifyResultTx(ctx, tx, row.ID, now, result.OK)
		if uErr != nil {
			return fmt.Errorf("update verify result: %w", uErr)
		}
		if n == 0 {
			// Concurrent state change — row was deleted between
			// SampleForVerify and this Tx. Skip audit (it would dangle to a
			// non-existent recording target) and surface the stale skip via
			// errStaleSkip; caller bumps the "stale" metric.
			return errStaleSkip
		}
		payload := map[string]any{
			"recording_id":  row.ID,
			"call_id":       row.CallID,
			"expected_sha":  result.ExpectedSHA,
			"actual_sha":    result.ActualSHA,
			"bytes_scanned": result.BytesScanned,
			"integrity_ok":  result.OK,
		}
		if aErr := writeAudit(ctx, tx, row.TenantID, row.ID, rapi.AuditActionVerified, payload, now); aErr != nil {
			return fmt.Errorf("audit: %w", aErr)
		}
		return nil
	})
	switch {
	case errors.Is(persistErr, errStaleSkip):
		p.metrics.IncIntegrityAction(tenantLabel, integrityResultStale)
		p.log.Debug("integrity verify skipped — row concurrently deleted",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
		)
		return
	case persistErr != nil:
		p.metrics.IncIntegrityAction(tenantLabel, integrityResultError)
		p.log.Warn("integrity persist failed; will retry next sweep",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
			zap.Error(persistErr),
		)
		return
	}

	if result.OK {
		p.metrics.IncIntegrityAction(tenantLabel, integrityResultOK)
		p.log.Info("integrity verify ok",
			zap.String("recording_id", row.ID.String()),
			zap.String("tenant_id", tenantLabel),
			zap.Int64("bytes_scanned", result.BytesScanned),
		)
		return
	}

	// Mismatch: bump both the per-row counter AND the dedicated failure
	// counter (master spec §15.5). The audit row already captured the
	// expected/actual sha pair so SOC operators can investigate.
	p.metrics.IncIntegrityAction(tenantLabel, integrityResultMismatch)
	p.metrics.IncIntegrityFailure(tenantLabel)
	p.log.Warn("integrity verify mismatch (sha256 disagreement)",
		zap.String("recording_id", row.ID.String()),
		zap.String("tenant_id", tenantLabel),
		zap.String("expected_sha", result.ExpectedSHA),
		zap.String("actual_sha", result.ActualSHA),
	)
}

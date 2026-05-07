package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Default values applied when RelayConfig fields are zero. Kept package-
// private so production callers must consult RelayConfig docs to know
// what they're getting.
const (
	defaultRelayBatchSize      = 100
	defaultRelayTick           = time.Second
	defaultRelayMaxRetry       = 10
	defaultRelayPublishTimeout = 5 * time.Second

	// maxLastErrorLen caps how much of an upstream error message we
	// persist on the row. Postgres TEXT has no hard limit but we keep
	// it tight for log hygiene and to avoid blowing up explain plans
	// that touch the column.
	maxLastErrorLen = 512
)

// Relay drains the event_outbox table to NATS via an eventbus.Publisher.
// One Relay per binary; it owns the goroutine for the lifetime of the
// process. Many replicas of the same binary may run in parallel —
// FOR UPDATE SKIP LOCKED in the drain query partitions the work
// without leader election.
//
// Delivery semantics: at-least-once. The Relay marks a row as published
// only after Publisher.Publish returns nil; on process crash mid-publish
// the row is re-delivered on the next pass.
type Relay struct {
	pool      *postgres.Pool
	publisher eventbus.Publisher
	cfg       RelayConfig
	logger    *zap.Logger
}

// RelayConfig parameterises Relay. Each binary that runs the relay
// pulls these from its config block (pkg/config.OutboxConfig).
type RelayConfig struct {
	// BatchSize bounds the number of events drained in one pass. Larger
	// batches amortise transaction overhead at the cost of higher tail
	// latency for the slowest event in the batch. Default 100.
	BatchSize int

	// Tick controls how often the relay polls when there is no work.
	// Under load the relay drains continuously; Tick only governs the
	// idle case. Default 1s.
	Tick time.Duration

	// MaxRetry caps the number of publish attempts per row. Once a
	// row's attempts >= MaxRetry the drain query skips it and the row
	// remains parked in the outbox for operator inspection via
	// last_error. Default 10.
	MaxRetry int

	// PublishTimeout bounds an individual Publisher.Publish call.
	// Default 5s.
	PublishTimeout time.Duration
}

func (c *RelayConfig) defaults() {
	if c.BatchSize <= 0 {
		c.BatchSize = defaultRelayBatchSize
	}
	if c.Tick <= 0 {
		c.Tick = defaultRelayTick
	}
	if c.MaxRetry <= 0 {
		c.MaxRetry = defaultRelayMaxRetry
	}
	if c.PublishTimeout <= 0 {
		c.PublishTimeout = defaultRelayPublishTimeout
	}
}

// NewRelay constructs a Relay backed by pool and publisher. The
// publisher must already be connected to its destination (e.g. NATS
// JetStream); the relay does not perform discovery.
//
// logger is required (non-nil); the relay logs publish failures and
// drain errors at warn/error levels. Callers that want silence pass
// zap.NewNop().
func NewRelay(pool *postgres.Pool, publisher eventbus.Publisher, cfg RelayConfig, logger *zap.Logger) *Relay {
	cfg.defaults()
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Relay{
		pool:      pool,
		publisher: publisher,
		cfg:       cfg,
		logger:    logger,
	}
}

// Run drives the relay loop until ctx is cancelled. It returns nil on
// graceful shutdown; non-nil only on initialisation errors before the
// loop starts. Drain errors inside the loop are logged and the loop
// continues — outbox is critical infrastructure and must keep trying.
//
// Run blocks. The canonical wiring is:
//
//	g.Go(func() error { return relay.Run(gctx) })
//
// inside an errgroup orchestrating the binary's long-running goroutines.
func (r *Relay) Run(ctx context.Context) error {
	if r.pool == nil {
		return errors.New("outbox: relay requires a non-nil pool")
	}
	if r.publisher == nil {
		return errors.New("outbox: relay requires a non-nil publisher")
	}

	r.logger.Info("outbox relay started",
		zap.Int("batch_size", r.cfg.BatchSize),
		zap.Duration("tick", r.cfg.Tick),
		zap.Int("max_retry", r.cfg.MaxRetry),
	)

	// time.NewTimer + Reset in the loop avoids the timer-leak per iteration
	// trap that bites time.After (golang-concurrency § BP6).
	timer := time.NewTimer(r.cfg.Tick)
	defer timer.Stop()

	for {
		// Drain everything currently pending before sleeping. This keeps
		// the relay responsive under bursts: when many rows arrive at
		// once we don't add Tick to every event's tail latency.
		if err := r.drainUntilEmpty(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				r.logger.Info("outbox relay stopping")
				return nil
			}
			r.logger.Error("outbox drain failed", zap.Error(err))
		}

		// Wait for either the tick or ctx cancellation. Reset before
		// re-arming the timer; receive from t.C once it fires.
		if !timer.Stop() {
			// Drain the channel only if a value is queued — Stop()
			// returns false either because the timer already fired
			// (and a value MAY be on the channel) or because we just
			// initialised it without a Reset since it last fired.
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(r.cfg.Tick)

		select {
		case <-ctx.Done():
			r.logger.Info("outbox relay stopping")
			return nil
		case <-timer.C:
			// loop
		}
	}
}

// drainUntilEmpty repeatedly drains a batch until a pass returns 0 rows.
// This lets bursts catch up without waiting for the next tick.
func (r *Relay) drainUntilEmpty(ctx context.Context) error {
	for {
		n, err := r.drainOnce(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		// Yield to ctx cancellation between batches.
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// drainOnce processes up to BatchSize pending rows in a single
// transaction. It returns the number of rows it touched (0 means no
// work was available). Errors abort the batch and are returned.
//
// The whole batch runs inside one BypassRLS transaction so the
// FOR UPDATE SKIP LOCKED locks are released only on commit/rollback —
// any other replica selecting at the same time skips this batch.
func (r *Relay) drainOnce(ctx context.Context) (int, error) {
	type pending struct {
		id      int64
		subject string
		payload []byte
	}

	var processed int
	err := r.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		const drainQ = `
			SELECT id, subject, payload
			FROM event_outbox
			WHERE published_at IS NULL AND attempts < $2
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED`
		rows, err := tx.Query(ctx, drainQ, r.cfg.BatchSize, r.cfg.MaxRetry)
		if err != nil {
			return fmt.Errorf("outbox: select pending: %w", err)
		}

		var batch []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.subject, &p.payload); err != nil {
				rows.Close()
				return fmt.Errorf("outbox: scan row: %w", err)
			}
			batch = append(batch, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("outbox: iterate rows: %w", err)
		}
		rows.Close()

		processed = len(batch)
		for _, p := range batch {
			r.handleRow(ctx, tx, p.id, p.subject, p.payload)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return processed, nil
}

// handleRow attempts to publish one row. On success it marks the row
// published; on failure it increments attempts and stores the error.
//
// Errors here are intentionally NOT propagated: a single bad row must
// not abort the batch (which would force every other row in the batch
// to wait for the next drain). The relay-level logger captures detail
// for operator triage; tests assert behaviour via DB state.
func (r *Relay) handleRow(ctx context.Context, tx postgres.Tx, id int64, subject string, payload []byte) {
	publishCtx, cancel := context.WithTimeout(ctx, r.cfg.PublishTimeout)
	defer cancel()

	pubErr := r.publisher.Publish(publishCtx, subject, payload)
	if pubErr == nil {
		const markQ = `UPDATE event_outbox
		               SET published_at = now(), last_error = NULL
		               WHERE id = $1`
		if _, err := tx.Exec(ctx, markQ, id); err != nil {
			r.logger.Error("outbox: mark published failed",
				zap.Int64("id", id),
				zap.String("subject", subject),
				zap.Error(err))
		}
		return
	}

	r.logger.Warn("outbox: publish failed; will retry",
		zap.Int64("id", id),
		zap.String("subject", subject),
		zap.Error(pubErr))

	const failQ = `UPDATE event_outbox
	               SET attempts = attempts + 1, last_error = $2
	               WHERE id = $1`
	if _, err := tx.Exec(ctx, failQ, id, truncate(pubErr.Error(), maxLastErrorLen)); err != nil {
		r.logger.Error("outbox: mark failure failed",
			zap.Int64("id", id),
			zap.String("subject", subject),
			zap.Error(err))
	}
}

// truncate clips s to at most n bytes — guards against pathologically
// long error strings (e.g. embedded base64 payload echoes) inflating
// the last_error column.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

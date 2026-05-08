package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// importStatusKey returns the Redis hash key used to back the
// ImportStatus DTO. Per-job, scoped to the crm namespace.
func importStatusKey(jobID string) string {
	return "crm:import:" + jobID
}

// ProgressTracker writes import status to Redis (durable, polled by
// GetImportStatus) and publishes the import.* NATS events (real-time
// dashboard updates). The two are coupled in one type so the handler
// only learns about one collaborator.
//
// Construction is via NewProgressTracker; both Redis and the publisher
// are optional — a nil Redis disables status hash maintenance, a nil
// publisher silently skips event publication. Tests inject either or
// both as fakes.
type ProgressTracker struct {
	rdb       redis.UniversalClient
	publisher eventbus.Publisher
	logger    *zap.Logger
	clock     func() time.Time
	ttl       time.Duration
}

// Compile-time guard that *ProgressTracker satisfies progressTracker.
// Keeps the interface stable; if the import handler grows new methods,
// the build fails until ProgressTracker catches up.
var _ progressTracker = (*ProgressTracker)(nil)

// NewProgressTracker builds a tracker with sensible defaults. A nil
// logger falls back to zap.NewNop. The Redis client is mandatory
// (without it status reads can't be served); publisher is optional —
// when nil, NATS events are skipped silently.
func NewProgressTracker(rdb redis.UniversalClient, publisher eventbus.Publisher, log *zap.Logger, clock func() time.Time) *ProgressTracker {
	if log == nil {
		log = zap.NewNop()
	}
	if clock == nil {
		clock = time.Now
	}
	return &ProgressTracker{
		rdb:       rdb,
		publisher: publisher,
		logger:    log,
		clock:     clock,
		ttl:       importStatusTTL,
	}
}

// hashFields keeps the Redis-side schema in one place — typo-prone
// repetition across Init/Update/Finish is the typical bug.
const (
	fieldStatus     = "status"
	fieldTotal      = "total"
	fieldProcessed  = "processed"
	fieldInserted   = "inserted"
	fieldSkipped    = "skipped"
	fieldStartedAt  = "started_at"
	fieldFinishedAt = "finished_at"
	fieldError      = "error"
)

// statePending / Running / Succeeded / Failed are the canonical status
// strings written into Redis. The string mirrors api.ImportStatus.State
// so the polling endpoint can emit them verbatim without translation.
const (
	stateQueued    = "queued"
	stateRunning   = "running"
	stateSucceeded = "succeeded"
	stateFailed    = "failed"
)

// Init writes the initial Redis hash with status=running and zeroed
// counters. The first call from Import (before enqueue) writes
// status=queued; the handler's call upgrades it to running once the
// task starts. Either is idempotent.
func (t *ProgressTracker) Init(ctx context.Context, jobID string, tenantID uuid.UUID, total int) error {
	if t.rdb == nil {
		return nil
	}
	now := t.clock().UTC().Format(time.RFC3339Nano)
	state := stateQueued
	if total > 0 {
		// Caller passed a known row count → the handler is now
		// running. zero total at Init time means "still queued, not
		// yet parsed".
		state = stateRunning
	}
	pipe := t.rdb.Pipeline()
	pipe.HSet(ctx, importStatusKey(jobID), map[string]any{
		fieldStatus:    state,
		fieldTotal:     total,
		fieldProcessed: 0,
		fieldInserted:  0,
		fieldSkipped:   0,
		fieldStartedAt: now,
	})
	pipe.Expire(ctx, importStatusKey(jobID), t.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("crm/service: progress init: %w", err)
	}
	return nil
}

// Update writes the running counter values and publishes the
// import.progress NATS event. Failures of the publish path are logged
// but do not fail the call — the Redis hash is the source of truth;
// NATS is the live-dashboard nice-to-have.
func (t *ProgressTracker) Update(ctx context.Context, jobID string, tenantID uuid.UUID, processed, inserted, skipped int) error {
	if t.rdb == nil {
		return nil
	}
	if err := t.rdb.HSet(ctx, importStatusKey(jobID), map[string]any{
		fieldStatus:    stateRunning,
		fieldProcessed: processed,
		fieldInserted:  inserted,
		fieldSkipped:   skipped,
	}).Err(); err != nil {
		return fmt.Errorf("crm/service: progress update: %w", err)
	}
	t.publish(ctx, api.SubjectImportProgressFor(tenantID), api.ImportProgressEvent{
		JobID:     jobID,
		Total:     0, // total stored separately; consumers re-fetch via Status if needed
		Processed: processed,
		Inserted:  inserted,
		Skipped:   skipped,
	})
	return nil
}

// Finish records the terminal success state and publishes the
// import.finished event.
func (t *ProgressTracker) Finish(ctx context.Context, jobID string, tenantID uuid.UUID, total, inserted, skipped int) error {
	if t.rdb == nil {
		return nil
	}
	now := t.clock().UTC().Format(time.RFC3339Nano)
	if err := t.rdb.HSet(ctx, importStatusKey(jobID), map[string]any{
		fieldStatus:     stateSucceeded,
		fieldTotal:      total,
		fieldProcessed:  total,
		fieldInserted:   inserted,
		fieldSkipped:    skipped,
		fieldFinishedAt: now,
	}).Err(); err != nil {
		return fmt.Errorf("crm/service: progress finish: %w", err)
	}
	t.publish(ctx, api.SubjectImportFinishedFor(tenantID), api.ImportFinishedEvent{
		JobID:    jobID,
		Total:    total,
		Inserted: inserted,
		Skipped:  skipped,
	})
	return nil
}

// Fail records the terminal failure state with the operator-facing
// error message. The msg should be low-cardinality (no per-row
// details) to keep log indices sane.
func (t *ProgressTracker) Fail(ctx context.Context, jobID string, tenantID uuid.UUID, errMsg string) error {
	if t.rdb == nil {
		return nil
	}
	now := t.clock().UTC().Format(time.RFC3339Nano)
	if err := t.rdb.HSet(ctx, importStatusKey(jobID), map[string]any{
		fieldStatus:     stateFailed,
		fieldFinishedAt: now,
		fieldError:      errMsg,
	}).Err(); err != nil {
		return fmt.Errorf("crm/service: progress fail: %w", err)
	}
	t.publish(ctx, api.SubjectImportFailedFor(tenantID), api.ImportFailedEvent{
		JobID: jobID,
		Error: errMsg,
	})
	return nil
}

// Status reads the Redis hash and translates it to the typed
// api.ImportStatus DTO. Returns ErrImportNotFound when the hash is
// absent (TTL elapsed or never created).
func (t *ProgressTracker) Status(ctx context.Context, jobID string) (*api.ImportStatus, error) {
	if t.rdb == nil {
		return nil, fmt.Errorf("crm/service: progress status: %w", api.ErrImportNotFound)
	}
	res, err := t.rdb.HGetAll(ctx, importStatusKey(jobID)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, fmt.Errorf("crm/service: progress status: %w", api.ErrImportNotFound)
		}
		return nil, fmt.Errorf("crm/service: progress status: %w", err)
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("crm/service: progress status: %w", api.ErrImportNotFound)
	}

	out := &api.ImportStatus{
		JobID:     jobID,
		State:     res[fieldStatus],
		Total:     atoiZero(res[fieldTotal]),
		Processed: atoiZero(res[fieldProcessed]),
		Inserted:  atoiZero(res[fieldInserted]),
		Skipped:   atoiZero(res[fieldSkipped]),
	}
	if v := res[fieldStartedAt]; v != "" {
		if ts, perr := time.Parse(time.RFC3339Nano, v); perr == nil {
			out.StartedAt = ts
		}
	}
	if v := res[fieldFinishedAt]; v != "" {
		if ts, perr := time.Parse(time.RFC3339Nano, v); perr == nil {
			t := ts
			out.FinishedAt = &t
		}
	}
	if msg := res[fieldError]; msg != "" {
		out.Errors = []api.ImportError{{Row: 0, Message: msg}}
	}
	return out, nil
}

// publish marshals payload and pushes it through the publisher. nil
// publisher is a no-op (composition root supplies a real one once
// NATS is wired in Plan 11).
func (t *ProgressTracker) publish(ctx context.Context, subject string, payload any) {
	if t.publisher == nil {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.logger.Warn("progress publish: marshal failed",
			zap.String("subject", subject), zap.Error(err))
		return
	}
	if err := t.publisher.Publish(ctx, subject, encoded); err != nil {
		t.logger.Warn("progress publish: send failed",
			zap.String("subject", subject), zap.Error(err))
	}
}

// atoiZero parses s as int; on parse failure or empty input, returns
// 0. Used to translate Redis-stored hash values back to ints.
func atoiZero(s string) int {
	if s == "" {
		return 0
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

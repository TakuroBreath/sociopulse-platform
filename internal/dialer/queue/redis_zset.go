package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// defaultTTL is the per-key TTL refreshed on every successful enqueue and
// requeue. 24h covers the longest realistic window between an enqueue and
// the eventual pop+dial: a 50k-respondent project with a low-throughput
// dialer might take a day to drain. Keys that age out cleanly is the
// safety net — a permanently abandoned project does not accumulate stale
// Redis state forever.
const defaultTTL = 24 * time.Hour

// Config bundles the dependencies and settings for a RedisQueue. Required
// fields (Redis) are documented per-field; nil-tolerated fields fall back
// to safe defaults so the constructor stays trivially wireable from tests.
type Config struct {
	// Redis is the connection used for the ZSET + dedup SET + Lua
	// scripts. Required.
	Redis *redis.Client

	// Logger receives per-method diagnostics. nil → zap.NewNop().
	Logger *zap.Logger

	// Clock returns the current time. nil → time.Now. Tests pass a
	// frozen clock so EnqueueRespondent yields deterministic
	// EnqueuedAt timestamps.
	Clock func() time.Time

	// TTL is the per-key TTL refreshed on every enqueue / requeue.
	// 0 → defaultTTL (24h).
	TTL time.Duration

	// Metrics is the per-package collector group. nil → no metrics
	// (the queue is fully functional without it).
	Metrics *Metrics
}

// RedisQueue implements api.CallQueue against a Redis ZSET + dedup SET
// pair per (tenant, project). All state-changing operations execute as
// atomic Lua scripts via the package-level handles in scripts.go.
type RedisQueue struct {
	rdb     *redis.Client
	log     *zap.Logger
	clock   func() time.Time
	ttl     time.Duration
	metrics *Metrics
}

// Compile-time interface check. Surfaces api.CallQueue signature drift
// the moment it happens (per Plan 09 lessons #8).
var _ api.CallQueue = (*RedisQueue)(nil)

// New constructs a RedisQueue. Returns an error when a required
// dependency is missing; nil-tolerated fields are filled with defaults
// so callers can pass an empty Config{Redis: rdb} for the simplest case.
func New(cfg Config) (*RedisQueue, error) {
	if cfg.Redis == nil {
		return nil, errors.New("queue.New: Redis is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &RedisQueue{
		rdb:     cfg.Redis,
		log:     logger,
		clock:   clock,
		ttl:     ttl,
		metrics: cfg.Metrics,
	}, nil
}

// zsetKey returns the canonical sorted-set key for the (tenant, project)
// pair. Stable across plans — Plan 10 retry orchestrator and Plan 11
// supervisor dashboard read from the same prefix.
func (q *RedisQueue) zsetKey(tenantID, projectID uuid.UUID) string {
	return "q:" + tenantID.String() + ":project:" + projectID.String()
}

// dedupKey returns the canonical dedup-set key for the (tenant, project)
// pair. Co-keyed with zsetKey so a Cluster deployment can hash-tag both
// into the same slot.
func (q *RedisQueue) dedupKey(tenantID, projectID uuid.UUID) string {
	return "qd:" + tenantID.String() + ":project:" + projectID.String()
}

// now returns q.clock() forced to UTC. Every queued item carries a UTC
// timestamp; the operator UI does the local-time rendering.
func (q *RedisQueue) now() time.Time { return q.clock().UTC() }

// EnqueueRespondent implements api.CallQueue. Adds a respondent to the
// queue. Returns ok=false (without error) when the respondent is already
// queued, per the interface contract.
//
// On success the dedup SET grows by one and the ZSET grows by one. The
// EnqueuedAt timestamp is bound by the queue's clock so the score is
// deterministic in tests; the AttemptN flows through unchanged for the
// retry orchestrator's per-attempt backoff.
func (q *RedisQueue) EnqueueRespondent(ctx context.Context, req api.EnqueueRequest) (bool, error) {
	priority := req.Priority
	if priority > maxPriority {
		priority = maxPriority
	}
	enqueuedAt := q.now()
	item := api.QueueItem{
		TenantID:     req.TenantID,
		ProjectID:    req.ProjectID,
		RespondentID: req.RespondentID,
		Priority:     priority,
		EnqueuedAt:   enqueuedAt,
		AttemptN:     req.AttemptN,
		Phone:        req.Phone,
		Region:       req.Region,
	}
	blob := encodeItem(item)
	scoreStr := strconv.FormatFloat(score(priority, enqueuedAt), 'f', -1, 64)
	res, err := enqueueScript.Run(
		ctx, q.rdb,
		[]string{q.zsetKey(req.TenantID, req.ProjectID), q.dedupKey(req.TenantID, req.ProjectID)},
		req.RespondentID.String(), scoreStr, string(blob), int(q.ttl.Seconds()),
	).Int()
	if err != nil {
		q.metrics.observeEnqueue(resultError)
		return false, fmt.Errorf("queue/enqueue: run script: %w", err)
	}
	if res == 0 {
		q.metrics.observeEnqueue(resultDuplicate)
		return false, nil
	}
	q.metrics.observeEnqueue(resultOK)
	return true, nil
}

// PickNext implements api.CallQueue. Atomically pops the highest-priority
// + oldest item via the pop_next.lua script. Returns api.ErrQueueEmpty
// when the ZSET is empty.
//
// The popped JSON blob is decoded back into an api.QueueItem in Go —
// the Lua script returns the bytes verbatim and only inspects the
// respondent_id field internally for the dedup SREM.
func (q *RedisQueue) PickNext(ctx context.Context, tenantID, projectID uuid.UUID) (api.QueueItem, error) {
	res, err := popNextScript.Run(
		ctx, q.rdb,
		[]string{q.zsetKey(tenantID, projectID), q.dedupKey(tenantID, projectID)},
	).Result()
	if errors.Is(err, redis.Nil) {
		// The script returns "" on empty queue; some go-redis versions
		// surface that as redis.Nil rather than a string. Treat both as
		// the empty-queue signal.
		q.metrics.observePickup(resultEmpty)
		return api.QueueItem{}, api.ErrQueueEmpty
	}
	if err != nil {
		q.metrics.observePickup(resultError)
		return api.QueueItem{}, fmt.Errorf("queue/pick: run script: %w", err)
	}
	blob, ok := res.(string)
	if !ok {
		q.metrics.observePickup(resultError)
		return api.QueueItem{}, fmt.Errorf("queue/pick: unexpected script result type %T", res)
	}
	if blob == "" {
		q.metrics.observePickup(resultEmpty)
		return api.QueueItem{}, api.ErrQueueEmpty
	}
	item, err := decodeItem([]byte(blob))
	if err != nil {
		q.metrics.observePickup(resultError)
		return api.QueueItem{}, fmt.Errorf("queue/pick: decode popped blob: %w", err)
	}
	q.metrics.observePickup(resultOK)
	return item, nil
}

// Requeue implements api.CallQueue. Re-inserts an item with the supplied
// delay applied to the new score. The item's Priority is capped at the
// maximum (9) so a runaway retry loop cannot escape the float-precision
// band; AttemptN is incremented BY THE CALLER (the retry orchestrator
// owns that semantics, not this layer). The new EnqueuedAt is the
// current clock plus the requested delay.
func (q *RedisQueue) Requeue(ctx context.Context, item api.QueueItem, delay time.Duration) error {
	if delay < 0 {
		delay = 0
	}
	priority := item.Priority
	if priority > maxPriority {
		priority = maxPriority
	}
	updated := item
	updated.Priority = priority
	updated.EnqueuedAt = q.now().Add(delay)
	blob := encodeItem(updated)
	scoreStr := strconv.FormatFloat(score(priority, updated.EnqueuedAt), 'f', -1, 64)
	if _, err := requeueScript.Run(
		ctx, q.rdb,
		[]string{q.zsetKey(item.TenantID, item.ProjectID), q.dedupKey(item.TenantID, item.ProjectID)},
		item.RespondentID.String(), scoreStr, string(blob), int(q.ttl.Seconds()),
	).Int(); err != nil {
		return fmt.Errorf("queue/requeue: run script: %w", err)
	}
	q.metrics.observeRequeue()
	return nil
}

// Size implements api.CallQueue. Returns the current ZSET cardinality
// for the (tenant, project) queue. As a side effect, updates the Size
// gauge so a metrics scrape reflects the latest reading.
func (q *RedisQueue) Size(ctx context.Context, tenantID, projectID uuid.UUID) (int64, error) {
	n, err := q.rdb.ZCard(ctx, q.zsetKey(tenantID, projectID)).Result()
	if err != nil {
		return 0, fmt.Errorf("queue/size: zcard: %w", err)
	}
	q.metrics.observeSize(tenantID.String(), projectID.String(), n)
	return n, nil
}

// Remove implements api.CallQueue. Atomically evicts a respondent from
// both the ZSET and the dedup SET via remove.lua. Idempotent — removing
// a missing entry is fine; both ZREM and SREM are no-ops on absence.
func (q *RedisQueue) Remove(ctx context.Context, tenantID, projectID, respondentID uuid.UUID) error {
	if _, err := removeScript.Run(
		ctx, q.rdb,
		[]string{q.zsetKey(tenantID, projectID), q.dedupKey(tenantID, projectID)},
		respondentID.String(),
	).Int(); err != nil {
		return fmt.Errorf("queue/remove: run script: %w", err)
	}
	return nil
}

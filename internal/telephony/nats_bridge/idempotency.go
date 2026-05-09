package nats_bridge //nolint:revive // package name mirrors the module's filesystem path

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// idempotencyKeyPrefix is the namespaced Redis key under which inbound
// command IDs are stored for deduplication. Stable across deployments —
// changing it would silently undo dedup for in-flight commands at upgrade.
const idempotencyKeyPrefix = "telephony:idempotency:"

// defaultIdempotencyTTL is the 24h dedup horizon used when callers pass a
// non-positive TTL. 24h covers the worst-case publisher-replay window —
// after that, replaying a command that long after submission is a bug
// elsewhere and we'd rather risk a re-execution than block forever on a
// crash-loop that backs up across days.
const defaultIdempotencyTTL = 24 * time.Hour

// IdempotencyGuard fronts a Redis SETNX key with a fixed TTL so a publisher
// crash + replay of the same commandID does not double-execute on the ESL
// fleet. The Plan 11.1 Task 4 contract: MarkSeen returns (true, nil) for a
// freshly seen ID, (false, nil) for a duplicate within TTL, and an error
// only when Redis itself fails — at which point the caller MUST NACK so
// the broker redelivers (silently dedup-failing on Redis-down would let
// commands be lost).
type IdempotencyGuard struct {
	rdb    redis.UniversalClient
	ttl    time.Duration
	logger *zap.Logger
}

// NewIdempotencyGuard constructs an IdempotencyGuard. rdb MUST be non-nil
// (a nil rdb is a wiring bug; panic surfaces it at boot rather than at
// first MarkSeen). ttl <= 0 falls back to defaultIdempotencyTTL (24h).
// logger nil-tolerated — falls back to zap.NewNop.
func NewIdempotencyGuard(rdb redis.UniversalClient, ttl time.Duration, logger *zap.Logger) *IdempotencyGuard {
	if rdb == nil {
		panic("nats_bridge: NewIdempotencyGuard: rdb required")
	}
	if ttl <= 0 {
		ttl = defaultIdempotencyTTL
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &IdempotencyGuard{rdb: rdb, ttl: ttl, logger: logger}
}

// MarkSeen records commandID under a TTL'd Redis key via SETNX and reports
// whether the caller should treat the command as new. Returns:
//
//   - (true, nil)  → new ID, caller dispatches.
//   - (false, nil) → duplicate within TTL, caller acks without dispatching.
//   - (_, err)     → Redis-side failure; caller MUST NACK so the broker
//     redelivers — silently dropping here would lose commands.
func (g *IdempotencyGuard) MarkSeen(ctx context.Context, commandID string) (bool, error) {
	key := idempotencyKeyPrefix + commandID
	ok, err := g.rdb.SetNX(ctx, key, "1", g.ttl).Result()
	if err != nil {
		return false, fmt.Errorf("nats_bridge: idempotency setnx: %w", err)
	}
	return ok, nil
}

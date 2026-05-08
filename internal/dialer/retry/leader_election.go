package retry

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/postgres"
)

// DefaultLockKey is the FNV-1a hash of "dialer.retry" cast to int64.
// Stable across replicas: every dialer worker computes the same key so
// pg_try_advisory_lock contends on a single leader slot.
//
// The value is computed once at package-init via fnvHash so the
// constant doc-comment can show what string seeded it without forcing
// the reader to compute the hash by hand.
var DefaultLockKey = fnvHash("dialer.retry")

// fnvHash computes the FNV-1a 64-bit hash of s and casts to int64
// (Postgres' pg_try_advisory_lock signature is bigint = int64).
func fnvHash(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	//nolint:gosec // intentional: pg advisory keys are bigint; reinterpret the unsigned hash.
	return int64(h.Sum64())
}

// PgLeader is a Postgres-advisory-lock-based leader election primitive.
// One replica per cluster holds the lock at a time; pg_try_advisory_lock
// is non-blocking, so peers see Acquire return ok=false rather than
// queueing.
//
// The lock is bound to the holding session — when the session
// disconnects (process crash, network blip, deliberate Release), PG
// drops the lock automatically and the next peer to call Acquire wins.
// This is the killer feature for leader election: no heartbeats, no
// expiration windows to tune; the TCP keepalive IS the leadership
// renewal.
//
// Goroutine safety: Acquire and Release are mutex-guarded so concurrent
// calls don't corrupt the held-conn pointer. In practice the
// orchestrator's Run loop is single-threaded; the lock is defence in
// depth for callers that compose PgLeader differently.
type PgLeader struct {
	pool *postgres.Pool
	key  int64
	log  *zap.Logger

	mu   sync.Mutex
	conn *postgres.Conn // nil when not currently leading
}

// NewPgLeader constructs a PgLeader. pool must be non-nil; logger nil
// falls back to zap.NewNop. key 0 falls back to DefaultLockKey.
func NewPgLeader(pool *postgres.Pool, key int64, logger *zap.Logger) (*PgLeader, error) {
	if pool == nil {
		return nil, errors.New("retry: PgLeader requires a postgres pool")
	}
	if key == 0 {
		key = DefaultLockKey
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &PgLeader{pool: pool, key: key, log: logger}, nil
}

// Key returns the advisory-lock key this leader instance contends on.
// Useful for the metrics gauge and for ops dashboards.
func (l *PgLeader) Key() int64 { return l.key }

// Acquire attempts to take the advisory lock. Returns ok=true when this
// instance now holds it; ok=false (without error) when a peer holds it.
// Errors are reserved for transport-level failures (pool exhausted,
// connection setup error).
//
// On a successful Acquire, this PgLeader holds an exclusive connection
// out of the pool until Release runs. The connection is the lock's
// session anchor: tearing it down releases the lock.
//
// Repeated Acquire calls without an intervening Release are idempotent
// — the second call returns ok=true without touching PG (we already
// hold the lock).
func (l *PgLeader) Acquire(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.conn != nil {
		// Already leading; idempotent.
		return true, nil
	}

	c, err := l.pool.LongLivedAcquire(ctx)
	if err != nil {
		return false, fmt.Errorf("retry/leader: acquire conn: %w", err)
	}

	var got bool
	if err := c.QueryRow(ctx, "select pg_try_advisory_lock($1)", l.key).Scan(&got); err != nil {
		// Release the conn so the pool isn't permanently shrunk by a
		// transient PG fault.
		c.Release()
		return false, fmt.Errorf("retry/leader: pg_try_advisory_lock: %w", err)
	}

	if !got {
		// A peer holds the lock. Return the connection to the pool so
		// other queries can use it; the next Acquire tick will pull a
		// fresh conn.
		c.Release()
		l.log.Debug("advisory lock held by peer",
			zap.Int64("key", l.key),
		)
		return false, nil
	}

	// We are now the leader. Hold the conn for the duration of
	// leadership; Release will free it.
	l.conn = c
	l.log.Info("acquired advisory lock — this instance is now retry leader",
		zap.Int64("key", l.key),
	)
	return true, nil
}

// Release relinquishes the advisory lock by returning the held
// connection to the pool. PG drops the lock automatically as soon as
// the session disconnects.
//
// Idempotent on a non-leading instance — Release is a no-op when no
// connection is held.
//
// We deliberately do NOT call pg_advisory_unlock: returning the conn
// is sufficient and keeps the lifecycle uniform with crash-time auto-
// release. (pg_advisory_unlock from a different session is silently
// rejected by PG, so coupling release to session shutdown removes a
// failure mode.)
func (l *PgLeader) Release(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.conn == nil {
		return
	}

	// Best-effort explicit unlock: pg_advisory_unlock returns false when
	// the lock is held by a different session, which can't happen here,
	// so we ignore the result. The Release of the conn below is what
	// guarantees cleanup even if the unlock RPC fails.
	if _, err := l.conn.Exec(ctx, "select pg_advisory_unlock($1)", l.key); err != nil {
		l.log.Debug("pg_advisory_unlock failed (ignored — releasing conn drops lock)",
			zap.Int64("key", l.key),
			zap.Error(err),
		)
	}

	l.conn.Release()
	l.conn = nil
	l.log.Info("released advisory lock", zap.Int64("key", l.key))
}

// IsLeading reports whether this instance currently holds the advisory
// lock. Used by the orchestrator to drive the leader_active gauge.
func (l *PgLeader) IsLeading() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

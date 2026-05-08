package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Account-lockout primitive backed by Redis. Spec §FR-A8 mandates a
// 15-minute lockout after 5 consecutive failed login attempts (consecutive
// = uninterrupted by a successful authentication). The Authenticator
// invokes RegisterFailure on each wrong-password / wrong-TOTP path and
// Reset on a successful authentication.
//
// Redis schema (also documented in plan-05-auth references):
//
//	auth:lk:fails:<uuid>   string (counter)  TTL=2*duration   consecutive failure count
//	auth:lk:until:<uuid>   string (unix-sec) TTL=duration     lock release timestamp
//
// fails-key TTL is intentionally 2× the lockout duration so a streak that
// pauses just shy of the threshold (e.g. 4 failures) automatically resets
// after a quiet period, preventing a years-old failure from contributing
// to a future lockout. The until-key TTL exactly matches the lockout
// duration so an expired lock auto-unblocks without explicit cleanup.

// Default knobs for NewLockoutRedis when callers pass zero.
const (
	defaultLockoutThreshold = 5
	defaultLockoutDuration  = 15 * time.Minute
)

// LockoutRedis is the Redis-backed implementation of Lockout.
//
// Concurrency: safe for concurrent use. RegisterFailure relies on Redis's
// atomic INCR — two concurrent failures cannot collude to skip the
// threshold check.
type LockoutRedis struct {
	rdb       redis.UniversalClient
	threshold int
	duration  time.Duration
	clock     func() time.Time
}

// Compile-time guarantee the implementation satisfies the public contract.
var _ Lockout = (*LockoutRedis)(nil)

// NewLockoutRedis constructs a LockoutRedis. Zero threshold/duration
// defaults to 5 failures / 15 minutes per spec §FR-A8. A nil clock falls
// back to time.Now.
func NewLockoutRedis(
	rdb redis.UniversalClient,
	threshold int,
	duration time.Duration,
	clock func() time.Time,
) *LockoutRedis {
	if threshold <= 0 {
		threshold = defaultLockoutThreshold
	}
	if duration <= 0 {
		duration = defaultLockoutDuration
	}
	if clock == nil {
		clock = time.Now
	}
	return &LockoutRedis{
		rdb:       rdb,
		threshold: threshold,
		duration:  duration,
		clock:     clock,
	}
}

// IsLocked reports whether the account is currently locked. The check is
// the until-key compared against the current clock, NOT the existence of
// the until-key — Redis TTLs are second-precision and a key that "should"
// have expired may linger for sub-second intervals after the wall-clock
// crosses its expiry. Comparing against the unix-second value lets the
// caller see the unlock as soon as the wall-clock passes the until.
func (l *LockoutRedis) IsLocked(ctx context.Context, id uuid.UUID) (bool, error) {
	v, err := l.rdb.Get(ctx, lockoutUntilKey(id)).Int64()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth/service: lockout get: %w", err)
	}
	return l.clock().Unix() < v, nil
}

// RegisterFailure increments the consecutive-failure counter. When the
// counter reaches the threshold it sets the until-key and returns
// locked=true so the caller can emit a single audit row per lockout
// transition. Subsequent failures during a lockout still increment the
// counter (and re-set the until-key, refreshing the unlock time): this
// matches the spec's intent that a flood of failures during a lockout
// does NOT shorten the lock — the until-key is overwritten with the same
// (or later) timestamp because the wall-clock has advanced.
func (l *LockoutRedis) RegisterFailure(ctx context.Context, id uuid.UUID) (bool, error) {
	failsKey := lockoutFailsKey(id)
	n, err := l.rdb.Incr(ctx, failsKey).Result()
	if err != nil {
		return false, fmt.Errorf("auth/service: lockout incr: %w", err)
	}

	// On the first INCR (when n==1) bind a TTL to the counter so an
	// uninterrupted streak naturally fades. Setting EXPIRE every call would
	// make a slow drumbeat of failures persist forever; pinning to the
	// first INCR makes the TTL anchor the start of the streak.
	if n == 1 {
		if err := l.rdb.Expire(ctx, failsKey, l.duration*2).Err(); err != nil {
			return false, fmt.Errorf("auth/service: lockout expire fails: %w", err)
		}
	}

	if n < int64(l.threshold) {
		return false, nil
	}

	until := l.clock().Add(l.duration).Unix()
	if err := l.rdb.Set(
		ctx,
		lockoutUntilKey(id),
		strconv.FormatInt(until, 10),
		l.duration,
	).Err(); err != nil {
		return false, fmt.Errorf("auth/service: lockout set until: %w", err)
	}
	return true, nil
}

// Reset zeroes the failure counter and clears any active lock. Called on
// successful authentication so a user with 4 prior failures who finally
// types the right password starts fresh on the next attempt.
func (l *LockoutRedis) Reset(ctx context.Context, id uuid.UUID) error {
	if err := l.rdb.Del(ctx, lockoutFailsKey(id), lockoutUntilKey(id)).Err(); err != nil {
		return fmt.Errorf("auth/service: lockout reset: %w", err)
	}
	return nil
}

// lockoutFailsKey returns auth:lk:fails:<uuid>. The string-counter holds
// the consecutive-failure count for the user.
func lockoutFailsKey(id uuid.UUID) string {
	return "auth:lk:fails:" + id.String()
}

// lockoutUntilKey returns auth:lk:until:<uuid>. The string holds the
// unix-second timestamp at which the lock auto-unblocks; presence of the
// key plus current-time < value means the account is locked.
func lockoutUntilKey(id uuid.UUID) string {
	return "auth:lk:until:" + id.String()
}

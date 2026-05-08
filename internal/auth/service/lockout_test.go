package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/auth/service"
)

// newLockoutT constructs a LockoutRedis bound to a fresh miniredis using
// the production threshold (5) and duration (15m). Tests that need
// different parameters can construct the LockoutRedis directly via
// service.NewLockoutRedis on top of newLockoutRedisClient.
func newLockoutT(
	t *testing.T,
	clock func() time.Time,
) (*service.LockoutRedis, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	lk := service.NewLockoutRedis(rdb, 5, 15*time.Minute, clock)
	return lk, mr
}

// 1. After 4 RegisterFailure -> IsLocked=false, locked=false on 4th.
func TestLockout_BelowThreshold_NotLocked(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	lk, _ := newLockoutT(t, clock)
	ctx := t.Context()

	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	for i := 0; i < 4; i++ {
		locked, err := lk.RegisterFailure(ctx, uid)
		require.NoError(t, err)
		assert.False(t, locked, "RegisterFailure %d should not lock", i+1)
	}

	isLocked, err := lk.IsLocked(ctx, uid)
	require.NoError(t, err)
	assert.False(t, isLocked, "should not be locked after 4 failures")
}

// 2. 5th RegisterFailure -> locked=true; subsequent IsLocked=true.
func TestLockout_AtThreshold_Locks(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	lk, _ := newLockoutT(t, clock)
	ctx := t.Context()

	uid := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	// 4 below threshold.
	for i := 0; i < 4; i++ {
		_, err := lk.RegisterFailure(ctx, uid)
		require.NoError(t, err)
	}

	// 5th -> locked.
	locked, err := lk.RegisterFailure(ctx, uid)
	require.NoError(t, err)
	assert.True(t, locked, "5th failure should lock")

	// IsLocked confirms.
	isLocked, err := lk.IsLocked(ctx, uid)
	require.NoError(t, err)
	assert.True(t, isLocked, "IsLocked should return true after lockout")
}

// 3. After 15 min (FastForward) the lock expires automatically.
func TestLockout_AutoUnlocksAfterDuration(t *testing.T) {
	t.Parallel()

	current := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return current }

	lk, mr := newLockoutT(t, clock)
	ctx := t.Context()

	uid := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	// Trigger lock.
	for i := 0; i < 5; i++ {
		_, err := lk.RegisterFailure(ctx, uid)
		require.NoError(t, err)
	}

	isLocked, err := lk.IsLocked(ctx, uid)
	require.NoError(t, err)
	require.True(t, isLocked)

	// Advance both clocks past the lockout duration.
	current = current.Add(16 * time.Minute)
	mr.FastForward(16 * time.Minute)

	isLocked, err = lk.IsLocked(ctx, uid)
	require.NoError(t, err)
	assert.False(t, isLocked, "should auto-unlock after duration")
}

// 4. Reset zeros the streak — IsLocked=false immediately.
func TestLockout_Reset_UnlocksAndZerosCounter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	lk, _ := newLockoutT(t, clock)
	ctx := t.Context()

	uid := uuid.MustParse("44444444-4444-4444-4444-444444444444")

	for i := 0; i < 5; i++ {
		_, err := lk.RegisterFailure(ctx, uid)
		require.NoError(t, err)
	}

	isLocked, err := lk.IsLocked(ctx, uid)
	require.NoError(t, err)
	require.True(t, isLocked)

	// Reset.
	require.NoError(t, lk.Reset(ctx, uid))

	isLocked, err = lk.IsLocked(ctx, uid)
	require.NoError(t, err)
	assert.False(t, isLocked, "Reset should unlock immediately")

	// Failure counter is also zeroed: 4 more failures should NOT trigger lock.
	for i := 0; i < 4; i++ {
		locked, err := lk.RegisterFailure(ctx, uid)
		require.NoError(t, err)
		assert.False(t, locked, "post-Reset failure %d should not lock", i+1)
	}
}

// 5. Two users are independent.
func TestLockout_Independence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	lk, _ := newLockoutT(t, clock)
	ctx := t.Context()

	uA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	uB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	// Lock A.
	for i := 0; i < 5; i++ {
		_, err := lk.RegisterFailure(ctx, uA)
		require.NoError(t, err)
	}

	isLockedA, err := lk.IsLocked(ctx, uA)
	require.NoError(t, err)
	assert.True(t, isLockedA)

	// B is unaffected.
	isLockedB, err := lk.IsLocked(ctx, uB)
	require.NoError(t, err)
	assert.False(t, isLockedB)
}

// 6. Failure counter window: 4 failures, 30 min idle, 5th failure starts a
// fresh streak (INCR re-creates from 1) and does NOT trigger lockout.
func TestLockout_StreakWindow_ResetsAfterIdle(t *testing.T) {
	t.Parallel()

	current := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return current }

	lk, mr := newLockoutT(t, clock)
	ctx := t.Context()

	uid := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	// 4 failures.
	for i := 0; i < 4; i++ {
		locked, err := lk.RegisterFailure(ctx, uid)
		require.NoError(t, err)
		require.False(t, locked)
	}

	// Idle past the failure-counter TTL (30 min).
	current = current.Add(31 * time.Minute)
	mr.FastForward(31 * time.Minute)

	// 5th failure now starts fresh (counter expired). Should NOT lock.
	locked, err := lk.RegisterFailure(ctx, uid)
	require.NoError(t, err)
	assert.False(t, locked, "fresh streak after idle should not lock on 1st failure")

	isLocked, err := lk.IsLocked(ctx, uid)
	require.NoError(t, err)
	assert.False(t, isLocked)
}

// 7. Default constructor parameters: threshold=0 -> 5; duration=0 -> 15min;
// nil clock -> time.Now.
func TestLockout_Defaults(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	lk := service.NewLockoutRedis(rdb, 0, 0, nil)
	require.NotNil(t, lk)

	ctx := context.Background()
	uid := uuid.MustParse("99999999-9999-9999-9999-999999999999")

	// Default threshold = 5: 4 failures shouldn't lock.
	for i := 0; i < 4; i++ {
		locked, err := lk.RegisterFailure(ctx, uid)
		require.NoError(t, err)
		assert.False(t, locked)
	}

	// 5th locks.
	locked, err := lk.RegisterFailure(ctx, uid)
	require.NoError(t, err)
	assert.True(t, locked)
}

// 8. Pipeline / network errors propagate from RegisterFailure and IsLocked.
func TestLockout_RedisErrorPropagates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	lk, mr := newLockoutT(t, clock)
	ctx := t.Context()

	mr.Close()

	uid := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")

	_, err := lk.RegisterFailure(ctx, uid)
	require.Error(t, err)

	_, err = lk.IsLocked(ctx, uid)
	require.Error(t, err)

	require.Error(t, lk.Reset(ctx, uid))
}

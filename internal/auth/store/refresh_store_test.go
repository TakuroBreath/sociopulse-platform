package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/auth/store"
)

// newRefreshStoreT spins up an in-memory miniredis, wires a real go-redis
// client to it, and returns a freshly constructed RefreshStore. The
// miniredis instance is auto-closed via t.Cleanup; the redis client is
// closed explicitly to avoid lingering goroutines that would trip
// goleak.VerifyTestMain.
func newRefreshStoreT(t *testing.T, ttl time.Duration) (*store.RefreshStore, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	return store.NewRefreshStore(rdb, ttl), mr
}

func sampleRecord() store.RefreshRecord {
	return store.RefreshRecord{
		UserID:    uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID:  uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		SessionID: "sid-fixed-1",
		ExpiresAt: time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC),
	}
}

// 1. Save -> Lookup round-trips JSON-encoded RefreshRecord.
func TestRefreshStore_SaveLookup_RoundTrips(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rs, _ := newRefreshStoreT(t, time.Hour)

	want := sampleRecord()
	require.NoError(t, rs.Save(ctx, "jti-1", want))

	got, err := rs.Lookup(ctx, "jti-1")
	require.NoError(t, err)
	assert.Equal(t, want.UserID, got.UserID)
	assert.Equal(t, want.TenantID, got.TenantID)
	assert.Equal(t, want.SessionID, got.SessionID)
	// JSON time round-trips at second precision; check UTC equality
	// rather than the underlying monotonic clock.
	assert.Equal(t, want.ExpiresAt.UTC(), got.ExpiresAt.UTC(),
		"ExpiresAt mismatch")
}

// 2. Lookup of an unknown jti returns ErrRefreshNotFound.
func TestRefreshStore_Lookup_UnknownReturnsNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rs, _ := newRefreshStoreT(t, time.Hour)

	_, err := rs.Lookup(ctx, "missing-jti")
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrRefreshNotFound, "expected ErrRefreshNotFound, got %v", err)
}

// 3. Rotate happy path: old key disappears, new key exists, rotated:<old>=newJTI.
func TestRefreshStore_Rotate_Happy(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rs, mr := newRefreshStoreT(t, time.Hour)

	rec := sampleRecord()
	require.NoError(t, rs.Save(ctx, "old-jti", rec))

	require.NoError(t, rs.Rotate(ctx, "old-jti", "new-jti", rec))

	// Old whitelist entry gone.
	_, err := rs.Lookup(ctx, "old-jti")
	assert.ErrorIs(t, err, store.ErrRefreshNotFound, "old jti should be gone, got %v", err)

	// New whitelist entry present.
	got, err := rs.Lookup(ctx, "new-jti")
	require.NoError(t, err)
	assert.Equal(t, rec.SessionID, got.SessionID)

	// rotated:<old> trail records new-jti.
	got2, err := mr.Get("auth:refresh:rotated:old-jti")
	require.NoError(t, err)
	assert.Equal(t, "new-jti", got2)
}

// 4. Rotate twice on the same oldJTI -> second call returns ErrRefreshAlreadyRotated.
func TestRefreshStore_Rotate_TwiceReturnsAlreadyRotated(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rs, _ := newRefreshStoreT(t, time.Hour)

	rec := sampleRecord()
	require.NoError(t, rs.Save(ctx, "old-jti", rec))
	require.NoError(t, rs.Rotate(ctx, "old-jti", "new-jti-A", rec))

	// Second rotation of the same oldJTI must fail.
	err := rs.Rotate(ctx, "old-jti", "new-jti-B", rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrRefreshAlreadyRotated, "expected ErrRefreshAlreadyRotated, got %v", err)
}

// 5. Delete makes Lookup return ErrRefreshNotFound; second Delete is a no-op.
func TestRefreshStore_Delete_IsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rs, _ := newRefreshStoreT(t, time.Hour)

	rec := sampleRecord()
	require.NoError(t, rs.Save(ctx, "jti-X", rec))

	require.NoError(t, rs.Delete(ctx, "jti-X"))
	_, err := rs.Lookup(ctx, "jti-X")
	assert.ErrorIs(t, err, store.ErrRefreshNotFound, "expected ErrRefreshNotFound after Delete, got %v", err)

	// Double-Delete is fine.
	require.NoError(t, rs.Delete(ctx, "jti-X"))
	require.NoError(t, rs.Delete(ctx, "never-saved"))
}

// 6. After ttl elapses (miniredis.FastForward), the saved entry is gone.
func TestRefreshStore_Save_RespectsTTL(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rs, mr := newRefreshStoreT(t, 30*time.Second)

	require.NoError(t, rs.Save(ctx, "jti-ttl", sampleRecord()))

	mr.FastForward(31 * time.Second)

	_, err := rs.Lookup(ctx, "jti-ttl")
	assert.ErrorIs(t, err, store.ErrRefreshNotFound, "expected ErrRefreshNotFound after TTL, got %v", err)
}

// Make sure the package never returns context.Canceled for a happy ctx —
// guards against a future refactor that swallows ctx incorrectly.
func TestRefreshStore_Save_AcceptsBackgroundCtx(t *testing.T) {
	t.Parallel()

	rs, _ := newRefreshStoreT(t, time.Hour)
	require.NoError(t, rs.Save(context.Background(), "jti-bg", sampleRecord()))
}

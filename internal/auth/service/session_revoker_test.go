package service_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/internal/auth/service"
)

// fakeNow returns a controllable clock function. Stored in an atomic so the
// test can advance the clock without races; the SessionRevoker reads time
// through the closure passed to NewSessionRevoker.
type fakeNow struct {
	nanos atomic.Int64
}

func newFakeNow(t time.Time) *fakeNow {
	c := &fakeNow{}
	c.nanos.Store(t.UnixNano())
	return c
}

func (c *fakeNow) Now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *fakeNow) Set(t time.Time)         { c.nanos.Store(t.UnixNano()) }
func (c *fakeNow) Func() func() time.Time  { return c.Now }
func (c *fakeNow) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

func newRevokerT(t *testing.T) (*service.SessionRevoker, *miniredis.Miniredis, *fakeNow) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := newFakeNow(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	return service.NewSessionRevoker(rdb, time.Hour, clk.Func()), mr, clk
}

// Compile-time guarantee the implementation satisfies the public contract.
var _ authapi.SessionRevoker = (*service.SessionRevoker)(nil)

// 1. RevokeSession then IsRevoked returns true.
func TestSessionRevoker_RevokeSession_Marks(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	r, _, _ := newRevokerT(t)

	require.NoError(t, r.RevokeSession(ctx, "sid-A"))

	got, err := r.IsRevoked(ctx, "sid-A", "any-jti")
	require.NoError(t, err)
	assert.True(t, got, "revoked sid should report IsRevoked=true")
}

// 2. RevokeAllForUser sets a cutoff; tokens with iat before cutoff are revoked.
func TestSessionRevoker_RevokeAllForUser_RespectsCutoff(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	r, _, clk := newRevokerT(t)

	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	// Token issued at T0 (one minute before cutoff).
	t0 := clk.Now().Add(-time.Minute)
	claims := authapi.Claims{
		UserID:    uid,
		SessionID: "sid-old",
		IssuedAt:  t0,
	}

	require.NoError(t, r.RevokeAllForUser(ctx, uid))

	got, err := r.IsRevokedClaims(ctx, claims)
	require.NoError(t, err)
	assert.True(t, got, "token issued before cutoff should be revoked")
}

// 2b. Boundary: a token whose IssuedAt equals the cutoff to the second
// MUST be revoked. The contract is "issued at or before the cutoff is
// out" — a token issued in the same nanosecond as the revocation is on
// the wrong side of the safety line.
func TestSessionRevoker_RevokeAllForUser_TokenAtCutoffBoundaryIsRevoked(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	r, _, clk := newRevokerT(t)

	uid := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	// Pin the clock so RevokeAllForUser writes a known cutoff. The
	// claims under test use the EXACT same Now() value as IssuedAt.
	cutoff := clk.Now()
	require.NoError(t, r.RevokeAllForUser(ctx, uid))

	claims := authapi.Claims{
		UserID:    uid,
		SessionID: "sid-boundary",
		IssuedAt:  cutoff, // same instant the cutoff was written
	}

	got, err := r.IsRevokedClaims(ctx, claims)
	require.NoError(t, err)
	assert.True(t, got, "token issued AT the cutoff must be revoked (iat <= cutoff)")
}

// 3. Tokens with iat after cutoff are not revoked.
func TestSessionRevoker_RevokeAllForUser_DoesNotRevokeNewerTokens(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	r, _, clk := newRevokerT(t)

	uid := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	// Set the cutoff first.
	require.NoError(t, r.RevokeAllForUser(ctx, uid))

	// New token issued AFTER the cutoff (clock advances by 1 minute).
	clk.Advance(time.Minute)
	claims := authapi.Claims{
		UserID:    uid,
		SessionID: "sid-new",
		IssuedAt:  clk.Now(),
	}

	got, err := r.IsRevokedClaims(ctx, claims)
	require.NoError(t, err)
	assert.False(t, got, "token issued after cutoff should NOT be revoked")
}

// 4. Unknown sid is not revoked, no error.
func TestSessionRevoker_IsRevoked_UnknownSidReturnsFalse(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	r, _, _ := newRevokerT(t)

	got, err := r.IsRevoked(ctx, "sid-unknown", "jti-unknown")
	require.NoError(t, err)
	assert.False(t, got, "unknown sid should report IsRevoked=false")
}

// 5. Bare IsRevoked (sid+jti) honours per-sid revocation.
func TestSessionRevoker_IsRevoked_PerSidShortCircuits(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	r, _, _ := newRevokerT(t)

	require.NoError(t, r.RevokeSession(ctx, "sid-revoked"))

	got, err := r.IsRevoked(ctx, "sid-revoked", "any-jti")
	require.NoError(t, err)
	assert.True(t, got)
}

//  6. RevokeSession's TTL matches the constructor's ttl arg (sanity check
//     via miniredis.TTL — guards against an accidental zero TTL that would
//     leak revocations forever).
func TestSessionRevoker_RevokeSession_HasTTL(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	r, mr, _ := newRevokerT(t)

	require.NoError(t, r.RevokeSession(ctx, "sid-ttl"))

	ttl := mr.TTL("auth:revoke:sid:sid-ttl")
	assert.Positive(t, ttl, "RevokeSession key must have a TTL")
}

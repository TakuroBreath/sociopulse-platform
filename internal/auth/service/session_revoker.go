package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	authapi "github.com/sociopulse/platform/internal/auth/api"
)

// userCutoffTTL is the lifetime of an auth:revoke:user:<id>:cutoff key.
// Long enough to outlive every refresh-token issued before the cutoff
// (refresh_ttl is 30 days in production), with a generous margin so a
// briefly-expired token cannot be revived after the cutoff is gone.
//
// The cutoff is intentionally NOT pinned to refresh_ttl from the
// constructor — RevokeAllForUser must remain effective even if the
// caller-provided ttl is short (e.g. test config). 30 days mirrors prod;
// SessionRevoker.NewSessionRevoker callers can override via NewSessionRevokerWithCutoff.
const userCutoffTTL = 30 * 24 * time.Hour

// revokerClient is the minimal redis surface SessionRevoker consumes.
// Exposed as an interface so tests can substitute miniredis-backed
// clients without crossing the depguard module boundary.
type revokerClient interface {
	redis.Cmdable
}

// SessionRevoker is the Redis-backed implementation of
// api.SessionRevoker. It tracks two kinds of revocation:
//
//  1. Per-session: auth:revoke:sid:<sid>     TTL=ttl   one-shot session kill.
//  2. Per-user cutoff: auth:revoke:user:<id>:cutoff   TTL=30d   tokens with
//     iat < cutoff are revoked. Used by force-logout-all (FR-A7).
//
// jti is accepted by IsRevoked for symmetry with api.SessionRevoker but
// not consulted: revocation operates at session-id granularity. Per-jti
// blacklisting would explode the keyspace and gain nothing beyond what
// the refresh whitelist already provides.
type SessionRevoker struct {
	rdb       revokerClient
	ttl       time.Duration
	cutoffTTL time.Duration
	now       func() time.Time
}

// Compile-time guarantee the implementation satisfies the public contract.
var _ authapi.SessionRevoker = (*SessionRevoker)(nil)

// NewSessionRevoker constructs a SessionRevoker. ttl is the lifetime of
// per-sid revocation keys (typically refresh_ttl, so a revoked session
// stays revoked for as long as a refresh token presented for it could be
// valid). The cutoff key uses a fixed userCutoffTTL.
//
// A nil clock falls back to time.Now. Tests inject a fake clock to
// exercise the "issued before cutoff" branch deterministically.
func NewSessionRevoker(rdb revokerClient, ttl time.Duration, clock func() time.Time) *SessionRevoker {
	if clock == nil {
		clock = time.Now
	}
	return &SessionRevoker{
		rdb:       rdb,
		ttl:       ttl,
		cutoffTTL: userCutoffTTL,
		now:       clock,
	}
}

// RevokeSession marks the session id as revoked with TTL = s.ttl.
func (r *SessionRevoker) RevokeSession(ctx context.Context, sid string) error {
	if sid == "" {
		return fmt.Errorf("auth/service: revoke session: sid required")
	}
	if err := r.rdb.Set(ctx, sidKey(sid), "1", r.ttl).Err(); err != nil {
		return fmt.Errorf("auth/service: revoke session: %w", err)
	}
	return nil
}

// RevokeAllForUser sets a cutoff timestamp for the user. Any access or
// refresh token issued at or before the cutoff is treated as revoked by
// IsRevokedClaims; tokens minted after the cutoff are unaffected.
//
// The cutoff is stored as a unix-second integer because Redis Get parsed
// into Int64 round-trips lossless across pods.
func (r *SessionRevoker) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return fmt.Errorf("auth/service: revoke all: userID required")
	}
	now := r.now().Unix()
	if err := r.rdb.Set(ctx, userCutoffKey(userID), strconv.FormatInt(now, 10), r.cutoffTTL).Err(); err != nil {
		return fmt.Errorf("auth/service: revoke all: %w", err)
	}
	return nil
}

// IsRevoked checks ONLY the per-sid key. The per-user cutoff cannot be
// honoured here because the api.SessionRevoker surface doesn't carry an
// IssuedAt. Callers that have a Claims value should use IsRevokedClaims
// for the full check; ValidateAccessToken in the Authenticator does so.
//
// Returning false on a missing per-sid key is the correct default —
// IsRevoked is OR'd with IsRevokedClaims at the call site.
func (r *SessionRevoker) IsRevoked(ctx context.Context, sid, _ string) (bool, error) {
	if sid == "" {
		return false, nil
	}
	exists, err := r.rdb.Exists(ctx, sidKey(sid)).Result()
	if err != nil {
		return false, fmt.Errorf("auth/service: is revoked: %w", err)
	}
	return exists > 0, nil
}

// IsRevokedClaims is the Claims-aware revocation check. It reports true
// when EITHER the sid is on the blacklist OR the user has a cutoff
// timestamp >= claims.IssuedAt. The Authenticator service uses this on
// every ValidateAccessToken / Refresh call.
//
// Errors short-circuit the check — a Redis outage must NOT silently
// admit revoked tokens. Callers should treat (false, err) as "fail
// closed" at the boundary.
func (r *SessionRevoker) IsRevokedClaims(ctx context.Context, c authapi.Claims) (bool, error) {
	// Per-sid kill switch first — it's a single Redis EXISTS call and the
	// most common positive case (operator force-logout from /me).
	if c.SessionID != "" {
		exists, err := r.rdb.Exists(ctx, sidKey(c.SessionID)).Result()
		if err != nil {
			return false, fmt.Errorf("auth/service: is revoked: %w", err)
		}
		if exists > 0 {
			return true, nil
		}
	}

	// Per-user cutoff. A missing key is treated as "no cutoff" (the
	// common case); only a Redis-side error fails the check.
	if c.UserID == uuid.Nil {
		return false, nil
	}
	cutoff, err := r.rdb.Get(ctx, userCutoffKey(c.UserID)).Int64()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth/service: is revoked: read cutoff: %w", err)
	}
	if c.IssuedAt.Unix() <= cutoff {
		return true, nil
	}
	return false, nil
}

// sidKey is the per-session blacklist key.
func sidKey(sid string) string { return "auth:revoke:sid:" + sid }

// userCutoffKey is the per-user cutoff timestamp key.
func userCutoffKey(id uuid.UUID) string {
	return "auth:revoke:user:" + id.String() + ":cutoff"
}

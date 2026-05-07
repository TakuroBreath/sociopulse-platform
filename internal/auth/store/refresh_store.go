package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RefreshRecord is the JSON-encoded value associated with each
// auth:refresh:<jti> key. It captures the minimum identity needed at refresh
// time so the Authenticator can mint a fresh access+refresh pair without
// going back to Postgres on the hot path.
//
// The struct is JSON-encoded into Redis directly. Time fields round-trip at
// second precision (RFC 3339) — sufficient for the refresh-token TTL window.
type RefreshRecord struct {
	UserID    uuid.UUID `json:"user_id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	SessionID string    `json:"sid"`
	ExpiresAt time.Time `json:"exp"`
}

// refreshStoreClient is the minimal subset of redis client behaviour
// RefreshStore consumes. Defining it here lets tests substitute mocks if
// needed (today they use a real redis.Client backed by miniredis), and it
// matches *redis.Client / redis.UniversalClient so production code passes
// the concrete type unchanged.
type refreshStoreClient interface {
	redis.Cmdable
	redis.Scripter
}

// RefreshStore is the Redis-backed whitelist of currently-valid refresh
// tokens, plus the rotation trail used for refresh-rotation reuse
// detection (OAuth 2.0 Security Best Current Practice §4.13).
//
// Schema:
//
//	auth:refresh:<jti>           string  TTL=ttl       JSON RefreshRecord
//	auth:refresh:rotated:<jti>   string  TTL=ttl       <new_jti>; replay-detection trail
//
// Rotate is implemented as a Lua script so the three operations
// (write rotated trail, delete old whitelist, write new whitelist) execute
// atomically — without it, a crash mid-rotation could leave a refresh token
// in a state where neither the old nor the new one is usable, or both are.
type RefreshStore struct {
	rdb refreshStoreClient
	ttl time.Duration
}

// rotateScript is the atomic rotation primitive. Semantics:
//
//	KEYS[1] = auth:refresh:rotated:<oldJTI>
//	KEYS[2] = auth:refresh:<oldJTI>
//	KEYS[3] = auth:refresh:<newJTI>
//	ARGV[1] = newJTI
//	ARGV[2] = ttl in seconds
//	ARGV[3] = JSON-encoded RefreshRecord
//
// Returns:
//
//	{2, ""}        — neither whitelist nor trail exists; oldJTI was never
//	                 issued (or both have expired). No state mutated.
//	{1, ""}        — first rotation; old key deleted, new key set, trail set.
//	{0, <prev>}    — replay detected; the rotated:<oldJTI> trail already
//	                 holds <prev>. No state mutated.
//
// SET ... EX <ttl> is used instead of PSETEX to avoid millisecond drift
// between the Redis lua-time and Go's time package. EX is sufficient for
// the refresh-token horizon (30 days, 1-second granularity).
var rotateScript = redis.NewScript(`
local prev = redis.call("GET", KEYS[1])
if prev then
    return {0, prev}
end
local cur = redis.call("EXISTS", KEYS[2])
if cur == 0 then
    return {2, ""}
end
redis.call("SET", KEYS[1], ARGV[1], "EX", ARGV[2])
redis.call("DEL", KEYS[2])
redis.call("SET", KEYS[3], ARGV[3], "EX", ARGV[2])
return {1, ""}
`)

// NewRefreshStore constructs a RefreshStore. ttl is the lifetime of every
// auth:refresh:<jti> entry; the rotation trail uses the same TTL because a
// trail is only meaningful while the corresponding refresh token could
// still be presented by an attacker — once the original would have expired
// anyway, a "replay" is a no-op.
//
// The rdb argument accepts any redis client that implements both Cmdable
// and Scripter (so *redis.Client, *redis.ClusterClient, and miniredis-
// backed clients all satisfy it).
func NewRefreshStore(rdb refreshStoreClient, ttl time.Duration) *RefreshStore {
	return &RefreshStore{rdb: rdb, ttl: ttl}
}

// Save records jti as a valid refresh token with TTL = s.ttl. Subsequent
// Lookup(jti) returns rec until the TTL elapses, the key is deleted via
// Delete, or the key is rotated.
func (s *RefreshStore) Save(ctx context.Context, jti string, rec RefreshRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("auth/store: marshal refresh record: %w", err)
	}
	if err := s.rdb.Set(ctx, refreshKey(jti), payload, s.ttl).Err(); err != nil {
		return fmt.Errorf("auth/store: save refresh: %w", err)
	}
	return nil
}

// Lookup returns the record stored under jti. ErrRefreshNotFound is
// returned (not wrapped, so errors.Is works) when the key is absent —
// every other error is wrapped with the package prefix.
func (s *RefreshStore) Lookup(ctx context.Context, jti string) (RefreshRecord, error) {
	raw, err := s.rdb.Get(ctx, refreshKey(jti)).Bytes()
	if errors.Is(err, redis.Nil) {
		return RefreshRecord{}, ErrRefreshNotFound
	}
	if err != nil {
		return RefreshRecord{}, fmt.Errorf("auth/store: lookup refresh: %w", err)
	}
	var rec RefreshRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return RefreshRecord{}, fmt.Errorf("auth/store: decode refresh record: %w", err)
	}
	return rec, nil
}

// Rotate atomically swaps the old refresh token for the new one. On success
// the rotation trail (auth:refresh:rotated:<oldJTI>) records newJTI and the
// old whitelist entry is deleted. On replay (oldJTI was already rotated)
// the function returns ErrRefreshAlreadyRotated; the caller is expected to
// revoke the entire session in that case.
func (s *RefreshStore) Rotate(ctx context.Context, oldJTI, newJTI string, rec RefreshRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("auth/store: marshal refresh record: %w", err)
	}

	keys := []string{
		rotatedKey(oldJTI),
		refreshKey(oldJTI),
		refreshKey(newJTI),
	}
	args := []any{newJTI, int(s.ttl.Seconds()), payload}

	res, err := rotateScript.Run(ctx, s.rdb, keys, args...).Result()
	if err != nil {
		return fmt.Errorf("auth/store: rotate refresh: %w", err)
	}

	arr, ok := res.([]any)
	if !ok || len(arr) < 1 {
		return fmt.Errorf("auth/store: rotate refresh: unexpected reply %T", res)
	}
	flag, ok := arr[0].(int64)
	if !ok {
		return fmt.Errorf("auth/store: rotate refresh: unexpected flag type %T", arr[0])
	}
	switch flag {
	case 0:
		return ErrRefreshAlreadyRotated
	case 2:
		return ErrRefreshNotFound
	}
	return nil
}

// Delete removes the whitelist entry for jti. Idempotent: calling Delete on
// an already-deleted (or never-saved) jti returns nil. The rotation trail
// is intentionally not removed — a session that was already rotated must
// continue to detect replays for its full TTL.
func (s *RefreshStore) Delete(ctx context.Context, jti string) error {
	if err := s.rdb.Del(ctx, refreshKey(jti)).Err(); err != nil {
		return fmt.Errorf("auth/store: delete refresh: %w", err)
	}
	return nil
}

// refreshKey is the auth:refresh:<jti> key that holds the JSON RefreshRecord.
func refreshKey(jti string) string { return "auth:refresh:" + jti }

// rotatedKey is the auth:refresh:rotated:<jti> trail used by Rotate to
// detect replay of an already-rotated jti.
func rotatedKey(jti string) string { return "auth:refresh:rotated:" + jti }

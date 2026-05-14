package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Cache is the narrow read-through-cache port the QueryService
// depends on. Mockable for unit tests; satisfied by *RedisCache in
// production.
//
// The contract is INTENTIONALLY non-fatal: a Get error is logged by
// the caller (QueryService) and treated as a miss, not propagated; a
// Set error is best-effort. This keeps the dashboard live even when
// Redis is degraded.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// RedisCache stores serialised query results in Redis with a per-call
// TTL. Values are gzip-compressed JSON; keys are
// "analytics:{tenant}:{method}:{q_hash}" — the caller builds the key
// via the cacheKey helper in query.go.
//
// nil-safe: a nil receiver and a RedisCache with a nil rdb both
// short-circuit Get/Set into clean miss/no-op without panicking. This
// matches the degraded-boot story where cmd/api may start without a
// Redis URL configured.
type RedisCache struct {
	rdb    redis.UniversalClient
	logger *zap.Logger
}

// Compile-time interface assertion — catches signature drift.
var _ Cache = (*RedisCache)(nil)

// NewRedisCache constructs a *RedisCache. The redis client may be
// nil — every method short-circuits cleanly on a nil rdb, matching
// the project-wide degraded-boot pattern (cmd/api logs a WARN at
// boot and proceeds without caching). logger nil-falls-back to a nop.
func NewRedisCache(rdb redis.UniversalClient, logger *zap.Logger) *RedisCache {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RedisCache{rdb: rdb, logger: logger}
}

// Get reads a gzip-compressed value from Redis under key, ungzips it,
// and returns the plain bytes. Cache misses (redis.Nil) return
// (nil, false, nil) — NOT an error. Any other transport / codec
// failure is wrapped and returned.
func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if c == nil || c.rdb == nil {
		return nil, false, nil
	}
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("analytics/cache: get %q: %w", key, err)
	}
	plain, err := gunzip(raw)
	if err != nil {
		return nil, false, fmt.Errorf("analytics/cache: gunzip %q: %w", key, err)
	}
	return plain, true, nil
}

// Set gzip-compresses value and writes it to Redis under key with the
// supplied TTL. nil-safe — a nil receiver / nil client / zero TTL
// short-circuit cleanly (in the TTL=0 case, Redis would persist
// forever, which is NOT a behaviour the caller ever wants here).
func (c *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	if ttl <= 0 {
		// Defensive: a zero TTL would mean "never expire" in Redis,
		// which violates the cache-eventual-freshness contract (Q6).
		// Short-circuit rather than write a poisoning entry.
		return nil
	}
	gzipped, err := gzipBytes(value)
	if err != nil {
		return fmt.Errorf("analytics/cache: gzip %q: %w", key, err)
	}
	if err := c.rdb.Set(ctx, key, gzipped, ttl).Err(); err != nil {
		return fmt.Errorf("analytics/cache: set %q: %w", key, err)
	}
	return nil
}

// gzipBytes round-trips b through a fresh gzip.Writer and returns the
// compressed payload. Used at Set time. A small (~32B) gzip header
// overhead means single-digit-byte payloads grow; the caller should
// not gzip-cache trivially small values, but the codec correctness
// holds for all inputs.
func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(b); err != nil {
		_ = gz.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gunzip reads gzipped bytes b and returns the decompressed payload.
// Used at Get time. A corrupt / truncated input returns a non-nil
// error; the caller treats it as a cache miss + fall-through to the
// origin (see QueryService.Calls).
//
// gzip.Reader.Close validates the CRC + length trailer — io.ReadAll
// alone does NOT consume the trailer, so a truncated payload could
// pass through silently if we deferred Close and ignored its error.
// Propagating the Close error makes truncated bytes a clean miss.
func gunzip(b []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	plain, readErr := io.ReadAll(gz)
	closeErr := gz.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return plain, nil
}

// noopCache is the local Cache implementation used by QueryService
// when the constructor receives a nil cache argument. Every Get is a
// miss; every Set is a successful no-op. This keeps the QueryService
// code path uniform without sprinkling nil checks throughout each
// method body.
type noopCache struct{}

// Compile-time interface assertion.
var _ Cache = noopCache{}

// Get always reports cache miss (false, nil).
func (noopCache) Get(_ context.Context, _ string) ([]byte, bool, error) {
	return nil, false, nil
}

// Set always succeeds without storing anything.
func (noopCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}

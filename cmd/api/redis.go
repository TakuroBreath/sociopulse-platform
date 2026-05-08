package main

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/config"
)

// redisPingTimeout is the deadline for the boot-time Ping that decides
// whether Redis-backed modules light up. Short on purpose — we don't
// want to block startup on a slow / unreachable Redis in dev/test.
const redisPingTimeout = 2 * time.Second

// openRedis builds a *redis.Client from cfg.Database.Redis and probes
// it with a short Ping. Both the client and the ping error are
// returned: callers decide whether to skip Redis-backed module
// registration based on the ping outcome.
//
// In dev/test where Redis is not running, this returns a non-nil
// client plus a non-nil error from Ping. The caller can defer
// rdb.Close() without a nil guard.
//
// When cfg.Database.Redis.Addr is empty, returns nil + a sentinel
// error — Redis is intentionally unwired (e.g. minimal smoke-test
// boot).
func openRedis(ctx context.Context, cfg config.Config, logger *zap.Logger) (*redis.Client, error) {
	if cfg.Database.Redis.Addr == "" {
		return nil, errors.New("database.redis.addr empty; Redis-backed modules disabled")
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Database.Redis.Addr,
		Password: cfg.Database.Redis.Password,
		DB:       cfg.Database.Redis.DB,
		PoolSize: cfg.Database.Redis.PoolSize,
	})

	pctx, cancel := context.WithTimeout(ctx, redisPingTimeout)
	defer cancel()
	if err := rdb.Ping(pctx).Err(); err != nil {
		logger.Warn("redis ping failed at boot",
			zap.String("addr", cfg.Database.Redis.Addr),
			zap.Error(err))
		return rdb, err
	}
	logger.Info("redis client open",
		zap.String("addr", cfg.Database.Redis.Addr),
		zap.Int("db", cfg.Database.Redis.DB),
		zap.Int("pool_size", cfg.Database.Redis.PoolSize))
	return rdb, nil
}

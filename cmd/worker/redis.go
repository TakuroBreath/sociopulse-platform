package main

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/config"
)

// redisPingTimeout caps the boot-time Redis Ping. Mirrors cmd/api.
const redisPingTimeout = 2 * time.Second

// openRedis builds a *redis.Client from cfg.Database.Redis and
// probes it. Returns the client + ping error so the caller can defer
// rdb.Close() unconditionally.
func openRedis(ctx context.Context, cfg config.Config, logger *zap.Logger) (*redis.Client, error) {
	if cfg.Database.Redis.Addr == "" {
		return nil, errors.New("database.redis.addr empty (worker requires Redis for the queue)")
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
		zap.Int("db", cfg.Database.Redis.DB))
	return rdb, nil
}

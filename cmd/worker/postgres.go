package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/postgres"
)

// pingTimeout caps the Ping that decides whether the worker proceeds.
// 2s mirrors cmd/api's choice — short enough that a wedged DB doesn't
// block kubelet's startup probe, long enough to absorb a busy-database
// momentary stall.
const pingTimeout = 2 * time.Second

// openPool initialises *postgres.Pool from cfg and probes it. Both
// the pool and the ping error are returned so the caller can defer
// pool.Close even on a failed Ping.
func openPool(ctx context.Context, cfg config.Config, logger *zap.Logger) (*postgres.Pool, error) {
	pool, err := postgres.Open(ctx, postgres.Config{
		DSN:               cfg.Database.Postgres.DSN,
		MaxConns:          int32(cfg.Database.Postgres.MaxConns), //nolint:gosec // bounded by config validation
		MinConns:          0,
		ConnectTimeout:    5 * time.Second,
		HealthCheckPeriod: cfg.Database.Postgres.MaxIdleTime,
	})
	if err != nil {
		logger.Error("postgres open failed", zap.Error(err))
		return nil, err
	}

	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		logger.Warn("postgres ping failed at boot", zap.Error(err))
		return pool, err
	}
	logger.Info("postgres pool open",
		zap.Int("max_conns", cfg.Database.Postgres.MaxConns))
	return pool, nil
}

package main

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/postgres"
)

// pingTimeout is the deadline for the boot-time Ping that decides
// whether the outbox relay starts. Short on purpose — we don't want
// to block startup on a slow / unreachable Postgres in dev/test.
const pingTimeout = 2 * time.Second

// openPool initialises *postgres.Pool from cfg and probes it with a
// short Ping. Both the pool and the ping error are returned: callers
// decide whether to fail boot, log, or skip relay startup based on the
// ping outcome.
//
// In dev/test where Postgres is not running, this returns a non-nil
// pool plus a non-nil error from Ping. The caller can defer pool.Close()
// without a nil guard (pkg/postgres.Pool.Close handles nil).
func openPool(ctx context.Context, cfg config.Config, logger *zap.Logger) (*postgres.Pool, error) {
	pool, err := postgres.Open(ctx, postgres.Config{
		DSN:               cfg.Database.Postgres.DSN,
		MaxConns:          int32(cfg.Database.Postgres.MaxConns), //nolint:gosec // bounded by config validation
		MinConns:          0,
		ConnectTimeout:    5 * time.Second,
		HealthCheckPeriod: cfg.Database.Postgres.MaxIdleTime,
	})
	if err != nil {
		// Open errors are real config problems (bad DSN, etc.). Log loudly
		// but don't kill cmd/api — the gateway is still useful for
		// /healthz / /metrics in degraded mode.
		logger.Error("postgres open failed",
			zap.String("dsn_redacted", redactDSN(cfg.Database.Postgres.DSN)),
			zap.Error(err))
		return nil, err
	}

	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		logger.Warn("postgres ping failed at boot",
			zap.String("dsn_redacted", redactDSN(cfg.Database.Postgres.DSN)),
			zap.Error(err))
		return pool, err
	}

	if err := assertAppPoolUser(ctx, pool, cfg.Service.Env, logger); err != nil {
		return pool, err
	}

	logger.Info("postgres pool open",
		zap.String("dsn_redacted", redactDSN(cfg.Database.Postgres.DSN)),
		zap.Int("max_conns", cfg.Database.Postgres.MaxConns))
	return pool, nil
}

// assertAppPoolUser is the boot-time defence against a misconfigured DSN
// connecting cmd/api as `tenancy_admin` (which has BYPASSRLS) instead of
// the intended `app` user. A regression here would silently disable tenant
// isolation on every query and is the kind of bug that only surfaces in a
// security review months later. The assertion runs once at startup and is
// O(1) — a single `select current_user` round-trip — so the cost is
// negligible.
//
// In `development` and `staging` we tolerate the pool connecting as a
// different user (testcontainers-postgres uses the `postgres` superuser by
// default) and only WARN. In `production` the assertion is fatal: returning
// a non-nil error here propagates up to openPool and refuses startup.
func assertAppPoolUser(ctx context.Context, pool *postgres.Pool, env string, logger *zap.Logger) error {
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	var user string
	if err := pool.RawQueryRow(pctx, "select current_user").Scan(&user); err != nil {
		return fmt.Errorf("postgres: verify pool user: %w", err)
	}
	if user == "app" {
		return nil
	}

	if env == "production" {
		return fmt.Errorf(
			"postgres: FATAL — app pool connected as %q, expected %q (refusing to start; tenancy_admin would silently bypass RLS)",
			user, "app")
	}
	logger.Warn("app pool connected as non-`app` user (acceptable in dev/test only)",
		zap.String("user", user),
		zap.String("env", env),
	)
	return nil
}

// redactDSN returns the DSN with the password segment replaced by *** so
// boot logs are safe to ship to a log aggregator. Best-effort: an
// unparseable DSN is logged as-is, since the open error already contains
// the parse failure.
func redactDSN(dsn string) string {
	// Find "://" then ":" before "@" — that's the password span.
	const sep = "://"
	idx := indexOf(dsn, sep)
	if idx < 0 {
		return dsn
	}
	rest := dsn[idx+len(sep):]
	at := indexOf(rest, "@")
	if at < 0 {
		return dsn
	}
	colon := indexOf(rest[:at], ":")
	if colon < 0 {
		return dsn
	}
	return dsn[:idx+len(sep)+colon+1] + "***" + dsn[idx+len(sep)+at:]
}

// indexOf is the tiniest substring search — kept here to avoid pulling
// in strings.Index for one-line use in redactDSN.
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

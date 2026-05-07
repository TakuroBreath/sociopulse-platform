package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config configures the Postgres connection pool. Most callers use
// Plan-02-defined config.Postgres and copy fields over.
type Config struct {
	// DSN is the libpq-style connection string, typically pointing at
	// PgBouncer ("postgres://app:pwd@pgbouncer:6432/sociopulse?sslmode=verify-full").
	DSN string

	// MaxConns is the upper bound on connections in the pool. Should be
	// chosen with PgBouncer's default_pool_size in mind: cluster-wide,
	// total Go-pool conns must be <= pgbouncer-default-pool-size * replicas.
	MaxConns int32

	// MinConns is the warm pool size. 1 is fine for dev; production usually
	// matches PgBouncer's min_pool_size.
	MinConns int32

	// ConnectTimeout caps individual connection-open attempts.
	ConnectTimeout time.Duration

	// HealthCheckPeriod is how often pgxpool pings idle conns. 0 means the
	// pgx default.
	HealthCheckPeriod time.Duration
}

// Pool is the application-facing pool. It does not expose pgxpool.Pool
// directly so we control how transactions begin.
type Pool struct {
	p *pgxpool.Pool
}

// Open initialises the pool. It applies SETUP statements once per new
// connection via AfterConnect (currently a no-op; placeholders remain for
// future timezone / search_path tweaks).
func Open(ctx context.Context, c Config) (*Pool, error) {
	if c.DSN == "" {
		return nil, errors.New("postgres: empty DSN")
	}
	cfg, err := pgxpool.ParseConfig(c.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse DSN: %w", err)
	}
	if c.MaxConns > 0 {
		cfg.MaxConns = c.MaxConns
	}
	if c.MinConns >= 0 {
		cfg.MinConns = c.MinConns
	}
	if c.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = c.ConnectTimeout
	}
	if c.HealthCheckPeriod > 0 {
		cfg.HealthCheckPeriod = c.HealthCheckPeriod
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// No-op: leave hook in place for future startup queries
		// (timezone, search_path, plan-cache size, ...).
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	return &Pool{p: pool}, nil
}

// Close drains and closes the pool. It is idempotent and safe on a nil
// receiver / nil underlying pool, so callers can `defer pool.Close()`
// after a failed Open without guarding the nil case.
func (p *Pool) Close() {
	if p == nil || p.p == nil {
		return
	}
	p.p.Close()
}

// Ping verifies the pool can talk to Postgres. Used for /readyz handlers.
func (p *Pool) Ping(ctx context.Context) error {
	if p == nil || p.p == nil {
		return errors.New("postgres: pool is not initialised")
	}
	return p.p.Ping(ctx)
}

// WithTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`.
// On nil error from fn, the tx commits; on any error, it rolls back.
//
// The tenant id is rendered safely via PostgreSQL's set_config builtin
// rather than string concatenation, because SET LOCAL does not accept
// parameterised values.
//
// uuid.Nil is rejected up front: an all-zero tenant id is never a real
// tenant, and an accidental zero-UUID combined with RLS that treats zero
// as "match all" would silently disable isolation.
func (p *Pool) WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	if tenantID == uuid.Nil {
		return errors.New("postgres: WithTenant requires a non-zero tenant id")
	}
	if p == nil || p.p == nil {
		return errors.New("postgres: pool is not initialised")
	}
	return p.transact(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			"select set_config('app.tenant_id', $1::text, true)", tenantID.String()); err != nil {
			return fmt.Errorf("postgres: set tenant: %w", err)
		}
		return fn(Tx{tx: tx})
	})
}

// BypassRLS runs fn inside a transaction whose role is tenancy_admin,
// which has BYPASSRLS. Reserved for the tenancy module's cross-tenant
// operations (Plan 04 Task 7) and migrations (cmd/migrator).
func (p *Pool) BypassRLS(ctx context.Context, fn func(Tx) error) error {
	if p == nil || p.p == nil {
		return errors.New("postgres: pool is not initialised")
	}
	return p.transact(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "set local role tenancy_admin"); err != nil {
			return fmt.Errorf("postgres: set role: %w", err)
		}
		return fn(Tx{tx: tx})
	})
}

// RawExec is an unscoped Exec for migrations and for testing only.
// Application code MUST go through WithTenant or BypassRLS.
func (p *Pool) RawExec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.p.Exec(ctx, sql, args...)
}

// RawQueryRow is similarly only for boot-time and tests.
func (p *Pool) RawQueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.p.QueryRow(ctx, sql, args...)
}

func (p *Pool) transact(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.p.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	// Rollback is a no-op once Commit succeeded. We deliberately use
	// context.Background here so the rollback still flushes if the
	// caller's ctx was cancelled — otherwise the connection would stay
	// in an aborted state until the pool reaped it. contextcheck would
	// have us thread ctx through, but that breaks the cleanup-on-
	// cancellation guarantee.
	defer func() { //nolint:contextcheck // intentional: rollback must run after caller-ctx cancellation
		_ = tx.Rollback(context.Background())
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

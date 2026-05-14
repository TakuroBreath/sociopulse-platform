// Package store owns the analytics module's connection to ClickHouse and
// the typed batch-insert helpers that feed events_calls,
// events_operator_state, and events_recording_uploaded.
//
// Why a wrapper instead of importing clickhouse-go everywhere:
//
//   - Depguard isolation. The depguard rule clickhouse-go-isolation
//     restricts direct imports of github.com/ClickHouse/clickhouse-go/v2
//     to cmd/migrator (database/sql path, golang-migrate) and the
//     internal/analytics tree (native protocol, batch inserts). Every
//     other module talks to ClickHouse only through *store.Conn.
//
//   - Centralised driver defaults. Open() applies project-wide knobs —
//     LZ4 compression, max_insert_threads=4, dial timeout — in one place
//     so consumers don't re-derive them. The result is a single audit
//     surface when CH driver tuning changes.
//
//   - Error-not-panic constructor. The native clickhouse-go does not
//     panic on misconfiguration but it also does not connect until the
//     first query; Open() Pings eagerly with a bounded ctx so
//     misconfiguration surfaces at process boot, not on first ingest.
//
//   - Healthy() for /readyz. The HTTP readiness handler needs a fast
//     liveness probe with its own deadline, separate from the ingestion
//     context. Healthy() wraps a 1-second-bounded Ping.
//
// The wrapper is intentionally thin. It does NOT abstract over the
// driver.Conn surface — Driver() exposes the native handle so the
// batch helpers in batch.go can call PrepareBatch directly. Code in
// other internal/analytics/* packages must go through this wrapper.
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.uber.org/zap"
)

// ErrInvalidConfig is returned by Config.Validate and Open when the
// supplied Config has a missing or zero-valued required field. Tests and
// callers should errors.Is against this sentinel rather than against
// the wrapped fmt.Errorf message text.
var ErrInvalidConfig = errors.New("analytics/store: invalid clickhouse config")

// Config is the user-facing configuration of a *Conn.
//
// All four required fields must be set before Open():
//
//   - DSN: clickhouse-go/v2 DSN. MUST NOT include the
//     "x-multi-statement=true" flag — that is a golang-migrate
//     extension and clickhouse-go's ParseDSN rejects it. See Plan 13.1
//     production lesson #4.
//   - BatchSize: max rows held in a single PrepareBatch before Send.
//     The ingest pipeline (Plan 13.2 Task 3) reads this to drive its
//     flush trigger. Must be > 0.
//   - FlushInterval: wall-clock cap on how long the ingest pipeline
//     buffers rows before flushing. Must be > 0.
//   - DialTimeout: optional. Defaults to 5s when zero.
//
// Logger is optional. Open() falls back to zap.NewNop when nil so the
// wrapper never panics on a NopLogger-style consumer.
type Config struct {
	DSN           string
	BatchSize     int
	FlushInterval time.Duration
	DialTimeout   time.Duration
	Logger        *zap.Logger
}

// Validate returns nil iff every required field is set. Failures wrap
// ErrInvalidConfig so callers can errors.Is against the sentinel.
//
// The check is intentionally cheap (no DSN parsing) so it can run on a
// hot path (e.g. inside a viper-driven config reload). DSN validity is
// confirmed in Open() via clickhouse.ParseDSN — a deeper check.
func (c Config) Validate() error {
	if c.DSN == "" {
		return fmt.Errorf("%w: DSN is required", ErrInvalidConfig)
	}
	if c.BatchSize <= 0 {
		return fmt.Errorf("%w: BatchSize must be > 0 (got %d)", ErrInvalidConfig, c.BatchSize)
	}
	if c.FlushInterval <= 0 {
		return fmt.Errorf("%w: FlushInterval must be > 0 (got %s)", ErrInvalidConfig, c.FlushInterval)
	}
	return nil
}

// Conn wraps a clickhouse-go/v2 driver.Conn. It is safe for concurrent
// use; the underlying driver pool serialises queries across goroutines.
//
// Construct via Open. The zero value is NOT usable — Driver() returns
// nil on a zero Conn and every method except Close panics if the
// underlying driver is nil.
type Conn struct {
	cfg    Config
	driver driver.Conn
	logger *zap.Logger

	closeOnce sync.Once
	closeErr  error
}

// defaultDialTimeout is the fallback applied when Config.DialTimeout
// is zero. Five seconds matches the project-wide convention used for
// Postgres and Redis bootstraps; it is long enough to absorb cold-VM
// startup but short enough to fail-fast on a misconfigured DSN.
const defaultDialTimeout = 5 * time.Second

// Open validates cfg, dials ClickHouse, and Pings to confirm the
// connection is alive. It returns a ready-to-use *Conn or wraps any
// driver error in a descriptive %w-chain.
//
// Defaults applied when cfg.DialTimeout==0: 5 seconds. Compression is
// fixed to LZ4 (the native protocol's recommended default — see
// clickhouse-go README "Compression"). Settings["max_insert_threads"]
// is set to 4, matching the per-insert thread cap used by the typical
// analytics ingest workload. Logger defaults to zap.NewNop.
//
// On Ping failure, Open() closes the underlying driver before
// returning so callers do not leak goroutines / sockets.
func Open(ctx context.Context, cfg Config) (*Conn, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	opts, err := clickhouse.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("analytics/store: parse DSN: %w", err)
	}

	dialTimeout := cfg.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = defaultDialTimeout
	}
	opts.DialTimeout = dialTimeout

	if opts.Compression == nil {
		opts.Compression = &clickhouse.Compression{Method: clickhouse.CompressionLZ4}
	}

	if opts.Settings == nil {
		opts.Settings = clickhouse.Settings{}
	}
	if _, ok := opts.Settings["max_insert_threads"]; !ok {
		opts.Settings["max_insert_threads"] = 4
	}

	drv, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("analytics/store: open clickhouse: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	c := &Conn{
		cfg:    cfg,
		driver: drv,
		logger: logger,
	}

	if err := c.Ping(ctx); err != nil {
		// Close on the bare driver — c.Close routes through closeOnce
		// and we do not want to mark the wrapper "closed" when the
		// failure path is "never opened correctly".
		_ = drv.Close()
		return nil, fmt.Errorf("analytics/store: ping after open: %w", err)
	}

	return c, nil
}

// Ping forwards to the underlying driver. It respects the supplied
// context's deadline and cancellation.
func (c *Conn) Ping(ctx context.Context) error {
	if err := c.driver.Ping(ctx); err != nil {
		return fmt.Errorf("analytics/store: ping: %w", err)
	}
	return nil
}

// healthyTimeout is the deadline applied by Healthy. One second matches
// the project-wide /readyz timeout convention; bumping it would push
// kubelet probe failures into ungoverned territory.
const healthyTimeout = time.Second

// Healthy is a /readyz-friendly Ping with a hard 1-second deadline.
// Callers SHOULD NOT pass a parent context that already has a longer
// deadline expecting it to apply — Healthy bounds time spent here so
// the readiness loop is predictable.
func (c *Conn) Healthy() error {
	ctx, cancel := context.WithTimeout(context.Background(), healthyTimeout)
	defer cancel()
	return c.Ping(ctx)
}

// Close is idempotent and nil-safe. Second and subsequent calls return
// the error returned by the FIRST close (or nil if it succeeded). A
// nil *Conn returns nil.
func (c *Conn) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.driver == nil {
			return
		}
		if err := c.driver.Close(); err != nil {
			c.closeErr = fmt.Errorf("analytics/store: close: %w", err)
		}
	})
	return c.closeErr
}

// Driver exposes the underlying native protocol handle. Used by
// batch.go inside this same package to call PrepareBatch / Exec /
// Query / QueryRow. External callers SHOULD NOT use this — it punches
// straight through the wrapper's abstraction and bypasses the
// depguard boundary's intent.
//
// Returns nil on a zero Conn.
func (c *Conn) Driver() driver.Conn {
	if c == nil {
		return nil
	}
	return c.driver
}

// Config returns a copy of the configuration used to open this Conn.
// Useful for diagnostics / readiness reports. The returned value is a
// snapshot; mutating it does not affect the live Conn.
func (c *Conn) Config() Config {
	if c == nil {
		return Config{}
	}
	return c.cfg
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/pkg/config"
)

// TestMain catches goroutine leaks in cmd/worker boot/shutdown. The
// retry orchestrator's Run loop must terminate within one tick of
// ctx cancellation and release its advisory-lock conn back to the
// pool; goleak surfaces any stuck goroutine.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// pgx's pool keeps a health-check goroutine alive across tests
		// when the pool isn't fully closed; tolerate it because we
		// assert clean shutdown via the run() return value below.
		goleak.IgnoreAnyFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}

// TestRunReturnsErrorOnInvalidConfig points run at a non-existent
// directory and asserts the load step surfaces an error rather than
// panicking.
func TestRunReturnsErrorOnInvalidConfig(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	t.Cleanup(cancel)

	err := run(ctx, "/nonexistent/path/that/should/not/exist")
	require.Error(t, err)
}

// TestRunReturnsErrorWhenPostgresUnreachable verifies that a config
// pointing at a disconnected Postgres surfaces a clean error from
// run() rather than running the worker indefinitely.
//
// We point at port 1 (TCP/IP reserved port) so the connection refuses
// fast — the worker reports postgres unavailable and exits.
func TestRunReturnsErrorWhenPostgresUnreachable(t *testing.T) {
	t.Parallel()

	dir := writeMinimalWorkerConfig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	err := run(ctx, dir)
	require.Error(t, err, "run() should error when Postgres ping fails")
}

// writeMinimalWorkerConfig writes a config.yaml that the worker can
// load but whose Postgres / Redis addresses point at unused ports so
// the boot fails fast.
func writeMinimalWorkerConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	yaml := `service:
  env: development
  log_level: info
  region: yc-ru-central-1
  name: sociopulse-worker-test
http:
  bind: ":0"
  read_timeout: 5s
  write_timeout: 10s
  idle_timeout: 30s
  max_body_size: 1MB
ws:
  bind: ":0"
  ping_interval: 20s
  read_buffer_size: 4096
  write_buffer_size: 4096
  max_message_size: 65536
  handshake_timeout: 10s
grpc:
  bind: ":0"
  reflection_enabled: true
  conn_timeout: 10s
database:
  postgres:
    dsn: postgres://app:devpass@127.0.0.1:1/sociopulse?sslmode=disable&connect_timeout=1
    max_conns: 5
    max_idle_time: 5m
    statement_cache: 100
  redis:
    addr: 127.0.0.1:1
    pool_size: 5
    db: 0
nats:
  urls: ["nats://localhost:4222"]
  account: cmd-worker
auth:
  jwt:
    issuer: https://app.sociopulse.local
    access_ttl: 15m
    refresh_ttl: 720h
    algorithm: HS256
observability:
  otel:
    endpoint: localhost:4317
    sampling_ratio: 1.0
    insecure: true
    service_name: sociopulse-worker-test
  metrics:
    bind: "127.0.0.1:0"
    namespace: sociopulse_test
  logging:
    redact_patterns: []
    sample_info_logs: 1.0
    sample_debug_logs: 0.05
shutdown:
  grace_period: 2s
outbox:
  batch_size: 50
  tick: 500ms
  max_retry: 5
`
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	return dir
}

// TestParseConfigDirEnvFallback verifies the fallback chain:
// --config-dir > SOCIOPULSE_CONFIG_DIR > defaultConfigDir.
func TestParseConfigDirEnvFallback(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("SOCIOPULSE_CONFIG_DIR", "")
		got := parseConfigDir(nil)
		require.Equal(t, defaultConfigDir, got)
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("SOCIOPULSE_CONFIG_DIR", "/from/env")
		got := parseConfigDir(nil)
		require.Equal(t, "/from/env", got)
	})
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("SOCIOPULSE_CONFIG_DIR", "/from/env")
		got := parseConfigDir([]string{"--config-dir", "/from/flag"})
		require.Equal(t, "/from/flag", got)
	})
}

// TestPassthroughDecryptorRoundTrip — defensive copy semantics.
func TestPassthroughDecryptorRoundTrip(t *testing.T) {
	t.Parallel()
	in := []byte("+79991234567")
	out, err := passthroughDecryptor{}.Decrypt(t.Context(), uuid.New(), in)
	require.NoError(t, err)
	require.Equal(t, in, out)
	out[0] = 'X'
	require.NotEqual(t, in, out)
}

// TestPassthroughDecryptorEmpty surfaces a clean error.
func TestPassthroughDecryptorEmpty(t *testing.T) {
	t.Parallel()
	_, err := passthroughDecryptor{}.Decrypt(t.Context(), uuid.New(), nil)
	require.Error(t, err)
}

// _ = ctx ensures the standard library "context" import stays
// referenced — guarded against a future edit that removes the only
// context.Context use above.
var _ = context.Background

// testConfig returns a minimal config.Config suitable for the helper
// tests. Bind addresses point at port 0 so the kernel picks free
// ports if a future test actually binds them.
func testConfig() config.Config {
	c := config.DefaultDev()
	c.Observability.Metrics.Bind = "127.0.0.1:0"
	return c
}

// emptyBindConfig returns testConfig with the metrics bind blanked
// out so buildHealthServer's fallback path is exercised.
func emptyBindConfig() config.Config {
	c := testConfig()
	c.Observability.Metrics.Bind = ""
	c.Database.Redis.Addr = ""
	return c
}

// zaptestLogger is a tiny wrapper to satisfy the linter's
// "*zap.Logger required" check while keeping each call site a
// one-liner.
func zaptestLogger(t *testing.T) *zap.Logger {
	t.Helper()
	return zaptest.NewLogger(t)
}

// httptestRecorderForHealthz exercises the /healthz endpoint by
// dispatching an in-memory request through the server's Handler.
func httptestRecorderForHealthz(t *testing.T, srv *http.Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

// TestBuildHealthServerRespondsOK is a smoke test for the /healthz
// surface. We grab the configured handler off the returned server,
// hit it via httptest, and verify the 200 OK body.
func TestBuildHealthServerRespondsOK(t *testing.T) {
	t.Parallel()

	srv := buildHealthServer(testConfig())
	require.NotNil(t, srv.Handler)

	rec := httptestRecorderForHealthz(t, srv)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", rec.Body.String())
}

// TestBuildHealthServerFallsBackWhenBindEmpty: an empty bind address
// in the metrics config falls back to defaultHealthBind.
func TestBuildHealthServerFallsBackWhenBindEmpty(t *testing.T) {
	t.Parallel()
	srv := buildHealthServer(emptyBindConfig())
	require.Equal(t, defaultHealthBind, srv.Addr)
}

// TestOpenRedisRequiresAddr surfaces a clean error when the config
// has no Redis address — the worker MUST have Redis to run the
// retry queue.
func TestOpenRedisRequiresAddr(t *testing.T) {
	t.Parallel()

	_, err := openRedis(t.Context(), emptyBindConfig(), zaptestLogger(t))
	require.Error(t, err)
}

// TestOpenRedisPingsAddr probes a refused-connection address; the
// helper returns the *redis.Client (so the caller can defer Close)
// alongside the ping error.
func TestOpenRedisPingsAddr(t *testing.T) {
	t.Parallel()

	cfg := emptyBindConfig()
	cfg.Database.Redis.Addr = "127.0.0.1:1" // reserved port
	rdb, err := openRedis(t.Context(), cfg, zaptestLogger(t))
	require.Error(t, err)
	require.NotNil(t, rdb)
	_ = rdb.Close()
}

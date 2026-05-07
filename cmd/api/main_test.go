package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestMain ensures cmd/api boot/shutdown does not leak goroutines.
//
// Tests boot a real *trace.TracerProvider whose OTLP exporter retries
// indefinitely against a missing collector at localhost:4317. On shutdown
// the batchSpanProcessor's drain blocks in the retry's wait() until the
// shutdown context expires, leaving short-lived OTel goroutines around when
// goleak inspects the runtime. We ignore those — they exit on their own
// once the retry's context deadline fires; in production with a reachable
// collector they exit promptly.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc/internal/retry.wait"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc.(*client).exportContext.func1"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).Shutdown.func1.1"),
	)
}

// TestRunStartsAndShutsDownCleanly drives the composition root through a full
// boot/shutdown cycle and asserts a clean exit. It picks free ports for HTTP
// and metrics listeners by writing a temporary config.yaml so other tests can
// run in parallel.
//
// This test does NOT exercise the outbox relay — the relay is a stub until
// Plan 03 Task 6 wires *pgxpool.Pool. See the integration smoke test deferred
// to Plan 03 (TestOutboxRelayStartsAndDrains).
func TestRunStartsAndShutsDownCleanly(t *testing.T) {
	t.Parallel()

	httpAddr := pickFreeAddr(t)
	metricsAddr := pickFreeAddr(t)
	configDir := writeMinimalDevConfig(t, httpAddr, metricsAddr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, configDir)
	}()

	// Wait for the HTTP listener to come up before declaring the boot done.
	requireListenerReady(t, httpAddr, 3*time.Second)

	// Hit /healthz to prove the gin engine is wired and serving.
	healthReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+httpAddr+"/healthz", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(healthReq)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Hit /metrics on the separate listener.
	metricsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+metricsAddr+"/metrics", nil)
	require.NoError(t, err)
	mresp, err := http.DefaultClient.Do(metricsReq)
	require.NoError(t, err)
	require.NoError(t, mresp.Body.Close())
	assert.Equal(t, http.StatusOK, mresp.StatusCode)

	// Trigger graceful shutdown via context cancellation.
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err, "run() should exit cleanly on context cancel")
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not exit within 5s of cancel")
	}
}

// TestRunReturnsErrorOnInvalidConfig points run at a non-existent directory and
// asserts the load step surfaces an error rather than panicking.
func TestRunReturnsErrorOnInvalidConfig(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	err := run(ctx, "/nonexistent/path/that/should/not/exist")
	require.Error(t, err)
}

// pickFreeAddr asks the kernel for a free TCP port, returns "127.0.0.1:N".
// The listener is closed immediately; race-prone in theory, fine in practice
// for serial test boots a few seconds apart.
func pickFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// requireListenerReady polls the address until something accepts a TCP connection.
func requireListenerReady(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if !errors.Is(err, context.DeadlineExceeded) && time.Now().After(deadline) {
			t.Fatalf("listener %s never came up: %v", addr, err)
		}
		<-tick.C
		if time.Now().After(deadline) {
			t.Fatalf("listener %s never came up", addr)
		}
	}
}

// writeMinimalDevConfig writes a config.yaml that boots cmd/api without any
// real backing services. The OTel endpoint stays at the default localhost:4317
// — the gRPC client is non-blocking (grpc.NewClient), so a missing collector
// only surfaces in batched export retries, not in startup.
func writeMinimalDevConfig(t *testing.T, httpBind, metricsBind string) string {
	t.Helper()
	dir := t.TempDir()
	yaml := `service:
  env: development
  log_level: info
  region: yc-ru-central-1
  name: sociopulse-api-test
http:
  bind: "` + httpBind + `"
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
    dsn: postgres://app:devpass@localhost:5432/sociopulse?sslmode=disable
    max_conns: 5
    max_idle_time: 5m
    statement_cache: 100
  redis:
    addr: localhost:6379
    pool_size: 5
    db: 0
nats:
  urls: ["nats://localhost:4222"]
  account: cmd-api
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
    service_name: sociopulse-api-test
  metrics:
    bind: "` + metricsBind + `"
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

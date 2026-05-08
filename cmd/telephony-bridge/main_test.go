package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestMain catches goroutine leaks introduced by the bridge boot path.
// We tolerate a handful of OTel exporter retry goroutines that block on
// missing collectors (matches the cmd/api ignore set — the gRPC client is
// non-blocking, so a downed collector leaves retry waits around when
// goleak inspects). Any other leaked goroutine is a regression.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc/internal/retry.wait"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc.(*client).exportContext.func1"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).Shutdown.func1.1"),
	)
}

// TestRunStartsAndShutsDownCleanly drives the composition root through a full
// boot/shutdown cycle and asserts /healthz, /metrics, and /readyz all answer
// while the bridge is up, then cancellation completes within the grace budget.
func TestRunStartsAndShutsDownCleanly(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)

	healthAddr := pickFreeAddr(t)
	metricsAddr := pickFreeAddr(t)
	configDir := writeMinimalDevConfig(t, mr.Addr(), metricsAddr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, runOptions{ConfigDir: configDir, HealthAddr: healthAddr})
	}()

	requireListenerReady(t, healthAddr, 3*time.Second)
	requireListenerReady(t, metricsAddr, 3*time.Second)

	// /healthz — liveness must always return 200 once the listener is up.
	healthBody, healthStatus := httpGet(t, ctx, "http://"+healthAddr+"/healthz")
	assert.Equal(t, http.StatusOK, healthStatus, "/healthz body: %s", healthBody)

	// /metrics — Prometheus exposition; should include the standard
	// process collector so operators see boot time. We read enough of the
	// body to assert on a representative metric name.
	metricsBody, metricsStatus := httpGet(t, ctx, "http://"+metricsAddr+"/metrics")
	assert.Equal(t, http.StatusOK, metricsStatus)
	assert.Contains(t, metricsBody, "process_start_time_seconds",
		"metrics body must include the standard process collector")

	// /readyz — with miniredis up but NATS unreachable (the dev config
	// points at localhost:4222 which RetryOnFailedConnect treats as
	// reconnecting — IsConnected returns false), the readiness probe
	// MUST return 503. This proves the NATS check is wired and not a
	// silent-pass.
	_, readyStatus := httpGet(t, ctx, "http://"+healthAddr+"/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, readyStatus,
		"/readyz must return 503 when NATS is disconnected")

	// Trigger graceful shutdown via context cancellation. The shutdown
	// budget in the test config is 2s; require a clean exit within 5s
	// to leave headroom for slow CI runners.
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err, "run() should exit cleanly on context cancel")
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not exit within 5s of cancel")
	}
}

// TestRunReturnsErrorOnInvalidConfig points run at a non-existent directory and
// asserts the load step surfaces an error rather than panicking. Mirrors the
// cmd/api guard so CI catches yaml-path regressions in either binary.
func TestRunReturnsErrorOnInvalidConfig(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	err := run(ctx, runOptions{
		ConfigDir:  "/nonexistent/path/that/should/not/exist",
		HealthAddr: pickFreeAddr(t),
	})
	require.Error(t, err)
}

// TestRunRejectsEmptyFSNodes proves the cfg.Telephony.Bridge.FSNodes guard
// fires before any HTTP listener comes up. Operators that ship a Helm
// values.yaml with `fs_nodes: []` get a clear boot failure rather than a
// silently-degraded bridge that cannot do anything useful.
func TestRunRejectsEmptyFSNodes(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)

	configDir := writeMinimalDevConfig(t, mr.Addr(), pickFreeAddr(t))
	// Strip the fs_nodes line so the validator catches an empty list.
	cfgPath := filepath.Join(configDir, "config.yaml")
	body, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	body = []byte(strings.ReplaceAll(string(body),
		"      esl_endpoint: \"127.0.0.1:8021\"\n",
		"",
	))
	body = []byte(strings.ReplaceAll(string(body),
		"    - id: fs-test\n",
		"",
	))
	require.NoError(t, os.WriteFile(cfgPath, body, 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err = run(ctx, runOptions{
		ConfigDir:  configDir,
		HealthAddr: pickFreeAddr(t),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fs_nodes")
}

// pickFreeAddr asks the kernel for a free TCP port, returns "127.0.0.1:N".
// The listener is closed immediately; race-prone in theory, fine in practice
// for serial test boots a few hundred milliseconds apart.
func pickFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// requireListenerReady polls the address until something accepts a TCP
// connection, then closes the socket. Mirrors cmd/api/main_test.go.
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

// httpGet performs a GET against url, returns body string + status code.
// On any error path we t.Fatal — the caller is asserting on the success
// path. This keeps test bodies readable.
func httpGet(t *testing.T, ctx context.Context, url string) (string, int) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body), resp.StatusCode
}

// writeMinimalDevConfig writes a config.yaml that boots cmd/telephony-bridge
// without any real backing services. NATS points at localhost:4222 (which
// RetryOnFailedConnect tolerates), Redis at the supplied miniredis addr.
//
// telephony.bridge.fs_nodes contains a single placeholder entry so the
// boot-time validation passes; the bridge subsystems themselves are stubs.
func writeMinimalDevConfig(t *testing.T, redisAddr, metricsBind string) string {
	t.Helper()
	dir := t.TempDir()
	yaml := `service:
  env: development
  log_level: info
  region: yc-ru-central-1
  name: telephony-bridge-test
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
    dsn: postgres://app:devpass@localhost:5432/sociopulse?sslmode=disable
    max_conns: 5
    max_idle_time: 5m
    statement_cache: 100
  redis:
    addr: "` + redisAddr + `"
    pool_size: 5
    db: 0
nats:
  urls: ["nats://127.0.0.1:14222"]
  account: telephony-bridge-test
auth:
  jwt:
    issuer: https://app.sociopulse.local
    access_ttl: 15m
    refresh_ttl: 720h
    algorithm: HS256
telephony:
  bridge:
    fs_nodes:
    - id: fs-test
      esl_endpoint: "127.0.0.1:8021"
    healthcheck_interval: 5s
    max_concurrent_per_node: 60
  trunks: []
  routing:
    default_strategy: least_cost_with_fallback
observability:
  otel:
    endpoint: localhost:4317
    sampling_ratio: 1.0
    insecure: true
    service_name: telephony-bridge-test
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

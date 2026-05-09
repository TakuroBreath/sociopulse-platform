//go:build integration

// main_integration_test.go drives the cmd/worker boot/shutdown
// against real Postgres 16 + Redis 7.4 containers. The unit test
// suite (main_test.go) verifies error paths against unreachable
// infra; this binary exercises the happy path so regressions in the
// composition-root wiring surface end-to-end.
//
// Build tag `integration` keeps the testcontainer overhead out of the
// default test run; CI invokes `go test -tags=integration ./...`.
package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestRunHappyPathStartsAndStops boots cmd/worker against real
// Postgres + Redis containers, lets it run for a couple of ticks,
// then cancels the parent ctx and asserts a clean exit.
//
// Not t.Parallel: testcontainers startup contention is real, and
// the integration suite already runs at modest concurrency by
// default.
func TestRunHappyPathStartsAndStops(t *testing.T) { //nolint:paralleltest // sequential is intentional
	pgDSN := startPostgres(t)
	redisAddr := startRedis(t)
	configDir := writeIntegrationConfig(t, pgDSN, redisAddr, false)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, configDir) }()

	// Wait for the orchestrator to log its startup. We can't easily
	// hook the log; instead we poll the /healthz endpoint until it
	// responds.
	healthOK := pollHealth(t, "127.0.0.1:0", 5*time.Second)
	if !healthOK {
		// The :0 path may pick a free port that we didn't capture.
		// In that case we fall back to time-based waiting — the boot
		// should be fast.
		time.Sleep(500 * time.Millisecond)
	}

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err, "run() should exit cleanly on context cancel")
	case <-time.After(10 * time.Second):
		t.Fatal("run() did not exit within 10s of cancel")
	}
}

// TestWorker_BootsWithoutRecording — Plan 12.4 Task 5. With
// recording.enabled=false (the dev default), buildRecordingWorkers
// returns an empty runner slice and the dialer retry orchestrator is
// the only registered Run-loop. Verifies `run` returns cleanly when
// the parent ctx cancels.
func TestWorker_BootsWithoutRecording(t *testing.T) { //nolint:paralleltest // testcontainers contention
	pgDSN := startPostgres(t)
	redisAddr := startRedis(t)
	configDir := writeIntegrationConfig(t, pgDSN, redisAddr, false /* recordingEnabled */)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, configDir) }()

	// Give the worker a tick to register every goroutine. Short pause
	// is enough — boot is fast against a real container.
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err, "run() should return cleanly on ctx cancel without recording")
	case <-time.After(10 * time.Second):
		t.Fatal("run() did not exit within 10s of cancel")
	}
}

// TestWorker_BootsWithRecording_Enabled — Plan 12.4 Task 5. With
// recording.enabled=true and a 64-hex-char local_keks entry, both the
// retention + integrity passes register against their respective
// advisory locks alongside the dialer retry orchestrator. Verifies all
// three goroutines drain on ctx cancel without leaks; the goleak
// VerifyTestMain in main_test.go enforces zero stuck goroutines.
func TestWorker_BootsWithRecording_Enabled(t *testing.T) { //nolint:paralleltest // testcontainers contention
	pgDSN := startPostgres(t)
	redisAddr := startRedis(t)
	configDir := writeIntegrationConfig(t, pgDSN, redisAddr, true /* recordingEnabled */)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, configDir) }()

	// Boot completion: each of the three Run-loops triggers an
	// immediate first sweep. 750ms is plenty for the registrations to
	// land but well under the retention 5min / integrity 1h tick so we
	// don't wait for a real sweep round.
	time.Sleep(750 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err, "run() should return cleanly on ctx cancel with recording workers enabled")
	case <-time.After(10 * time.Second):
		t.Fatal("run() did not exit within 10s of cancel (recording workers stuck?)")
	}
}

// startPostgres boots a Postgres 16 container, runs every migration
// in the repo, and returns the connection DSN.
func startPostgres(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("sociopulse"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	migrationsURL := repoMigrationsURL(t)
	mig, err := migrate.New(migrationsURL, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = mig.Close() })
	require.NoError(t, mig.Up())
	return dsn
}

// startRedis boots Redis 7.4 in a container and returns its host:port.
func startRedis(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req := testcontainers.ContainerRequest{
		Image:        "redis:7.4-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)
	return host + ":" + port.Port()
}

// repoMigrationsURL returns the file:// URL of the repo's migrations
// dir.
func repoMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err)
	return "file://" + abs
}

// writeIntegrationConfig writes a config.yaml that points cmd/worker
// at the supplied Postgres + Redis addresses. recordingEnabled toggles
// the recording.enabled + local_keks block so a single helper drives
// both the no-recording (Plan 10) and recording-on (Plan 12.4 Task 5)
// boot paths.
func writeIntegrationConfig(t *testing.T, pgDSN, redisAddr string, recordingEnabled bool) string {
	t.Helper()
	dir := t.TempDir()
	// 64-hex-char (32-byte) AES-256 KEK — fixed dev seed so the boot is
	// deterministic. Production deploys configure this via the YAML
	// secret-manager path.
	const devKEK = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	recordingBlock := ""
	if recordingEnabled {
		recordingBlock = `recording:
  enabled: true
  local_keks:
    kek-it: "` + devKEK + `"
  workers:
    retention_interval: 5m
    retention_batch: 100
    integrity_interval: 1h
    integrity_batch: 10
    integrity_sample_percent: 1.0
`
	}
	yaml := `service:
  env: development
  log_level: info
  region: yc-ru-central-1
  name: sociopulse-worker-it
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
    dsn: ` + pgDSN + `
    max_conns: 5
    max_idle_time: 5m
    statement_cache: 100
  redis:
    addr: ` + redisAddr + `
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
    service_name: sociopulse-worker-it
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
` + recordingBlock
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	return dir
}

// pollHealth attempts to GET /healthz on the supplied address until
// the deadline expires. Returns true on a successful 200, false when
// the deadline expires without a successful response.
//
// Today the worker's /healthz binds to an addr we can't easily
// capture (port :0 in the config); the helper exists for symmetry
// with cmd/api's flow and is a no-op when the addr resolves to :0.
func pollHealth(_ *testing.T, addr string, timeout time.Duration) bool {
	if addr == "" {
		return false
	}
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var success atomic.Bool
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			success.Store(true)
			return true
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		<-tick.C
	}
	return success.Load()
}

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fullDevYAML = `
service:
  env: development
  log_level: debug
  region: yc-ru-central-1
  name: sociopulse-api
http:
  bind: ":8080"
  read_timeout: 10s
  write_timeout: 30s
  idle_timeout: 120s
  max_body_size: 10MB
ws:
  bind: ":8081"
  ping_interval: 20s
  read_buffer_size: 4096
  write_buffer_size: 4096
  max_message_size: 65536
  handshake_timeout: 10s
grpc:
  bind: ":9091"
  reflection_enabled: true
  conn_timeout: 10s
database:
  postgres:
    dsn: postgres://app:devpass@localhost:5432/sociopulse?sslmode=disable
    max_conns: 20
    max_idle_time: 5m
    statement_cache: 100
  redis:
    addr: localhost:6379
    pool_size: 20
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
    service_name: sociopulse-api
  metrics:
    bind: ":9090"
    namespace: sociopulse
  logging:
    redact_patterns:
      - 'phone:\+?7\d{10}'
      - 'token:\w+'
    sample_info_logs: 1.0
    sample_debug_logs: 0.05
shutdown:
  grace_period: 15s
`

func writeYAML(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600))
}

func TestLoadFromDirSucceeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, fullDevYAML)
	snap, err := Load(LoadOptions{Dir: dir})
	require.NoError(t, err)
	c := snap.Get()
	assert.Equal(t, "development", c.Service.Env)
	assert.Equal(t, ":8080", c.HTTP.Bind)
	assert.Equal(t, 15*time.Second, c.Shutdown.GracePeriod)
	assert.Equal(t, int64(10*1024*1024), c.HTTP.MaxBodySize)
}

func TestLoadFailsOnInvalidEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, `service:
  env: staging-snowflake
  log_level: debug
  region: yc-ru-central-1
http:
  bind: ":8080"
  read_timeout: 10s
  write_timeout: 30s
  idle_timeout: 120s
  max_body_size: 10MB
ws:
  bind: ":8081"
  ping_interval: 20s
  max_message_size: 65536
  handshake_timeout: 10s
grpc:
  bind: ":9091"
  conn_timeout: 10s
database:
  postgres: { dsn: x }
  redis: { addr: x }
nats:
  urls: ["x"]
  account: cmd-api
observability:
  metrics: { bind: ":9090", namespace: sociopulse }
shutdown:
  grace_period: 15s
`)
	_, err := Load(LoadOptions{Dir: dir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "env must be one of")
}

func TestLoadEmptyDirErrors(t *testing.T) {
	t.Parallel()
	_, err := Load(LoadOptions{Dir: ""})
	require.Error(t, err)
}

func TestLoadFileMissingFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // empty directory, no config.yaml
	snap, err := Load(LoadOptions{Dir: dir})
	require.NoError(t, err, "Load with empty dir should fall back to DefaultDev seeds")
	require.NotNil(t, snap)
	t.Cleanup(func() { _ = snap.Close() })

	cfg := snap.Get()
	assert.Equal(t, "development", cfg.Service.Env)
	assert.Equal(t, ":8080", cfg.HTTP.Bind)
	assert.Equal(t, ":9090", cfg.Observability.Metrics.Bind)
	assert.Equal(t, "yc-ru-central-1", cfg.Service.Region)
	assert.NotEmpty(t, cfg.NATS.URLs, "NATS URLs should be seeded from DefaultDev")
	assert.NotEmpty(t, cfg.Database.Postgres.DSN, "Postgres DSN should be seeded from DefaultDev")
}

// TestLoadRealDevConfig reads configs/development/config.yaml from the
// repository root. It is a smoke test that proves the on-disk dev fixture
// stays in sync with the Config struct shape.
func TestLoadRealDevConfig(t *testing.T) {
	t.Parallel()
	dir := findRepoConfigsDir(t, "configs", "development")
	snap, err := Load(LoadOptions{Dir: dir})
	require.NoError(t, err)
	c := snap.Get()
	assert.Equal(t, "development", c.Service.Env)
	assert.Equal(t, ":8080", c.HTTP.Bind)
	assert.Equal(t, ":9091", c.GRPC.Bind)
	assert.Equal(t, ":9090", c.Observability.Metrics.Bind)
	assert.Equal(t, "sociopulse", c.Observability.Metrics.Namespace)
	assert.Equal(t, int64(10*1024*1024), c.HTTP.MaxBodySize)
	assert.Equal(t, 15*time.Second, c.Shutdown.GracePeriod)
}

// findRepoConfigsDir walks up from the test working directory until it finds
// the requested configs/<env> path. We do not hardcode an absolute path so the
// test works from a worktree, sandbox, or fresh clone.
func findRepoConfigsDir(t *testing.T, segments ...string) string {
	t.Helper()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		candidate := filepath.Join(append([]string{dir}, segments...)...)
		if _, err := os.Stat(filepath.Join(candidate, "config.yaml")); err == nil {
			return candidate
		}
	}
	t.Fatalf("could not find %s in any parent of %s", filepath.Join(segments...), cwd)
	return ""
}

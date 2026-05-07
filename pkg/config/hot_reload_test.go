package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHotReloadReplacesSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, fullDevYAML)
	snap, err := Load(LoadOptions{Dir: dir, HotReload: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = snap.Close() })

	sub := snap.Subscribe()
	assert.Equal(t, ":8080", snap.Get().HTTP.Bind)

	// Rewrite config.yaml with a different HTTP bind.
	updated := `
service:
  env: development
  log_level: debug
  region: yc-ru-central-1
  name: sociopulse-api
http:
  bind: ":18181"
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(updated), 0o600))

	select {
	case c := <-sub:
		assert.Equal(t, ":18181", c.HTTP.Bind)
	case <-time.After(5 * time.Second):
		t.Fatal("hot-reload did not fire within 5s")
	}
}

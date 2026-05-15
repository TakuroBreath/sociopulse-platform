//go:build smoke

package smoke

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// WriteSmokeConfig writes a config.yaml for cmd/api into t.TempDir()
// and returns the directory. The config points at the testcontainer
// DSNs in stack and uses httpBind / metricsBind for the HTTP + metrics
// listeners.
//
// Mirrors configs/development/config.yaml's shape but with the
// production-bound fields swapped out for testcontainer values:
//
//   - database.postgres.dsn → stack.PostgresDSN
//   - database.redis.addr   → stack.RedisAddr  (host:port form)
//   - nats.urls             → [stack.NATSURL]
//   - http.bind             → httpBind
//   - observability.metrics.bind → metricsBind
//   - kms.provider          → local (per Plan 21 references § 4.6)
//   - s3.provider           → local (same)
//   - auth.jwt.secret       → deterministic smoke value (per § 4.5)
//   - shutdown.grace_period → 2s (snappy test teardown)
//   - analytics.enabled     → false (smoke does not run ClickHouse)
//
// The file mode is 0o600 to match the production config-mounting
// convention.
func WriteSmokeConfig(t *testing.T, stack *Stack, httpBind, metricsBind string) string {
	t.Helper()
	dir := t.TempDir()
	yaml := smokeConfigYAML(stack, httpBind, metricsBind)
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600), "smoke: write config.yaml")
	return dir
}

// smokeConfigYAML is the textual config template. Split out so the
// values that vary per stack are obvious. Hard-coded fields mirror
// configs/development/config.yaml — the smoke environment is "as much
// like dev as possible while pointing at real containers".
func smokeConfigYAML(stack *Stack, httpBind, metricsBind string) string {
	return `service:
  env: development
  log_level: info
  region: yc-ru-central-1
  name: sociopulse-api-smoke
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
    dsn: ` + stack.PostgresDSN + `
    max_conns: 8
    max_idle_time: 5m
    statement_cache: 100
  redis:
    addr: ` + stack.RedisAddr + `
    pool_size: 8
    db: 0
nats:
  urls: ["` + stack.NATSURL + `"]
  account: cmd-api
auth:
  jwt:
    issuer: https://app.sociopulse.local
    access_ttl: 15m
    refresh_ttl: 720h
    algorithm: HS256
    # Deterministic smoke secret so auth.Module.Register doesn't
    # WARN-skip on empty Config.Auth.JWT.Secret. Documentary only —
    # production loads via Lockbox per ADR-0001.
    secret: smoke-test-secret-do-not-use-in-prod
kms:
  provider: local
  local_key_hex: "4242424242424242424242424242424242424242424242424242424242424242"
  # Plan 21b Task 3 — pre-register the deterministic smoke KEK that
  # SeedTenantAndAdmin writes into tenants.kms_kek_id ("smoke-kek-default")
  # so tenancy.KMSResolver.Encrypt finds the key without first calling
  # KMSClient.CreateKey (the smoke seed bypasses TenantService.Create
  # via direct SQL inserts). Without this, the crm import handler stalls
  # at the first phone-encrypt with ErrKEKNotFound and the job never
  # reaches the "succeeded" terminal state.
  #
  # The hex value is "abcd" repeated 16 times = 32 bytes after decode,
  # deterministic across smoke runs. Mirrors the existing
  # recording.local_keks shape (same kek_id, same hex) so a single id
  # works for both the crm import path AND the recording-stream path.
  local_keks:
    smoke-kek-default: "abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
s3:
  provider: local
  bucket_prefix: sociopulse-smoke-recordings-
recording:
  # Plan 21b Task 1 — register the deterministic smoke KEK that
  # SeedTenantAndAdmin assigns to every tenant ("smoke-kek-default")
  # so cmd/api's recwire.LocalPorts builds a LocalDEKUnwrapper that
  # recognises the id. The hex value is "abcd" repeated 16 times = 32
  # bytes after decode, matching tests/smoke/recording_seed.go's
  # smokeKEKHex. A drift here breaks the recording-stream scenario
  # with "unknown KMS key" before any HTTP roundtrip.
  local_keks:
    smoke-kek-default: "abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
observability:
  otel:
    endpoint: localhost:4317
    sampling_ratio: 1.0
    insecure: true
    service_name: sociopulse-api-smoke
  metrics:
    bind: "` + metricsBind + `"
    namespace: sociopulse_smoke
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
analytics:
  # Smoke harness does not boot a ClickHouse container. Plan 21 Task 4
  # only exercises Health/Auth/RBAC/TenantIsolation scenarios; analytics
  # readback is out of scope for Phase 1.
  enabled: false
  batch_size: 1
  flush_interval: 1s
  dedup_lru_size: 1
  cache_short_ttl: 1s
  cache_long_ttl: 1m
  long_window_threshold: 24h
  queue_group: analytics-smoke
  drain_timeout: 1s
`
}

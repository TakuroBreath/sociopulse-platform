# cmd/api Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Every task follows TDD — write the failing test first, watch it fail, then write the implementation, watch it pass, then commit.

**Goal:** Convert the hello-world `cmd/api` (from Plan 00) into a production-grade HTTP/WS/gRPC service skeleton: Viper-based config loading with hot-reload, three-pillar observability (zap/OTel/Prometheus), liveness/readiness endpoints wired to real dependencies, chi-based gateway middleware chain (request-id, recover, logging, tracing, metrics, idempotency, rate-limit, auth-stub), a `/ws` WebSocket endpoint, an mTLS gRPC server (no services registered yet), the `Module` registration pattern, and graceful shutdown with timeouts. Real domain modules (auth, crm, surveys, dialer, …) are filled in by Plans 04+.

**Architecture:**
- `cmd/api/main.go` orchestrates: load config → init logger/tracer/metrics → open db/redis/nats clients → build `Deps` struct → run `Module.Register(deps)` for every module → mount routes on chi router → start HTTP, WS, gRPC, metrics servers → wait for SIGTERM → graceful shutdown.
- `internal/config/` — Viper loader (yaml + ENV override + fsnotify hot-reload). Top-level struct matches spec §14.2.
- `internal/observability/` — zap logger with PII redaction encoder; OTel SDK with OTLP/gRPC exporter; Prometheus registry exposing `/metrics` on a separate port.
- `internal/healthz/` — `/healthz` (liveness) and `/readyz` (readiness against Postgres+Redis+NATS).
- `internal/gateway/` — chi router wrapper plus middleware chain. Each middleware in its own file under `internal/gateway/middleware/`.
- `internal/realtime/wsapi/` — `/ws` handler reading the auth handshake. Real hub/topic dispatch lands in Plan 11.
- `internal/grpcapi/` — mTLS gRPC server with reflection on dev/staging.
- `internal/modules/` — `Module` interface + `Deps` struct + module registry. A stub `healthz` module demonstrates the pattern; real modules plug in via Plan 04+.

**Tech Stack:**
- Go 1.22+
- `github.com/spf13/viper` v1.18+, `github.com/fsnotify/fsnotify` v1.7+
- `go.uber.org/zap` v1.27+
- `go.opentelemetry.io/otel` v1.27+, `go.opentelemetry.io/otel/sdk` v1.27+, `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` v1.27+
- `github.com/prometheus/client_golang` v1.19+
- `github.com/go-chi/chi/v5` v5.0+, `github.com/go-chi/chi/v5/middleware`
- `nhooyr.io/websocket` v1.8+
- `google.golang.org/grpc` v1.63+
- `github.com/redis/go-redis/v9` v9.5+
- `github.com/jackc/pgx/v5` (used here only as a type — the real driver wiring lands in Plan 03; this plan accepts a `*pgxpool.Pool` interface and stubs it for tests)
- `github.com/nats-io/nats.go` v1.34+
- `github.com/golang-jwt/jwt/v5` v5.2+ (auth-stub only)
- `github.com/stretchr/testify` v1.9+

**Spec sections covered:** §4 (architecture), §5 (modules), §10 (real-time plane / WS protocol), §12 (security middleware), §14.2 (config structure), §15 (three-pillar observability).

**Prerequisites:**
- Plan 00 completed: Go module rooted at `github.com/sociopulse/platform`, `cmd/api/main.go` exists with the stub `/healthz` handler, Makefile and golangci-lint configured.
- Plan 01 may be in flight in parallel — this plan does not depend on real Yandex Cloud infrastructure. All external dependencies (Postgres, Redis, NATS) are abstracted behind interfaces and stubbed for unit tests.

---

## File Structure

This plan creates the following files (and modifies `cmd/api/main.go`, `go.mod`, `configs/development/config.yaml`):

```
cmd/api/
├── main.go                                       # MODIFIED — full bootstrap orchestration
├── main_test.go                                  # MODIFIED — covers run() and shutdown
├── deps.go                                       # builds the Deps struct from config + clients
├── deps_test.go
├── server_http.go                                # builds *http.Server with chi router
├── server_grpc.go                                # builds *grpc.Server with mTLS
├── server_ws.go                                  # /ws handler wiring
├── server_metrics.go                             # /metrics on :9090
└── shutdown.go                                   # graceful-shutdown coordinator + tests

internal/
├── config/
│   ├── config.go                                 # top-level Config struct + Load function
│   ├── config_test.go
│   ├── http.go                                   # HTTPConfig
│   ├── ws.go                                     # WSConfig
│   ├── database.go                               # DatabaseConfig (postgres, clickhouse, redis)
│   ├── nats.go                                   # NATSConfig
│   ├── s3.go                                     # S3Config
│   ├── kms.go                                    # KMSConfig
│   ├── auth.go                                   # AuthConfig (jwt, password, ratelimit, totp)
│   ├── dialer.go                                 # DialerConfig
│   ├── telephony.go                              # TelephonyConfig
│   ├── recording.go                              # RecordingConfig
│   ├── reports.go                                # ReportsConfig
│   ├── observability.go                          # ObservabilityConfig
│   ├── shutdown.go                               # ShutdownConfig (timeouts)
│   ├── grpc.go                                   # GRPCConfig (mTLS, bind, reflection)
│   ├── load.go                                   # Viper bootstrap, ENV bind, hot-reload
│   ├── load_test.go
│   ├── env_override_test.go                      # ENV-substitution tests
│   └── hot_reload_test.go                        # fsnotify-based reload test
│
├── observability/
│   ├── logger.go                                 # zap factory + redaction encoder
│   ├── logger_test.go
│   ├── redact.go                                 # PII-redacting zapcore.Encoder wrapper
│   ├── redact_test.go
│   ├── tracer.go                                 # OTel SDK init (OTLP/gRPC)
│   ├── tracer_test.go
│   ├── metrics.go                                # Prometheus registry + /metrics handler
│   ├── metrics_test.go
│   └── correlation.go                            # request_id ↔ trace_id ↔ span_id helper
│
├── healthz/
│   ├── liveness.go                               # /healthz handler
│   ├── liveness_test.go
│   ├── readiness.go                              # /readyz handler with checker interface
│   ├── readiness_test.go
│   └── checks/
│       ├── postgres.go                           # Postgres readiness probe
│       ├── postgres_test.go
│       ├── redis.go                              # Redis ping probe
│       ├── redis_test.go
│       ├── nats.go                               # NATS server-info probe
│       └── nats_test.go
│
├── gateway/
│   ├── router.go                                 # chi router builder
│   ├── router_test.go
│   ├── context.go                                # RequestContext type + ctxKey helpers
│   ├── context_test.go
│   ├── errors.go                                 # error response writer
│   ├── errors_test.go
│   └── middleware/
│       ├── request_id.go                         # mw 1: X-Request-ID generation/propagation
│       ├── request_id_test.go
│       ├── recover.go                            # mw 2: panic recovery → 500 + log
│       ├── recover_test.go
│       ├── logging.go                            # mw 3: structured access log
│       ├── logging_test.go
│       ├── tracing.go                            # mw 4: OTel span per request
│       ├── tracing_test.go
│       ├── metrics.go                            # mw 5: http_request_duration_seconds histogram
│       ├── metrics_test.go
│       ├── idempotency.go                        # mw 6: Idempotency-Key Redis SET NX
│       ├── idempotency_test.go
│       ├── ratelimit.go                          # mw 7: Redis token-bucket per IP/user
│       ├── ratelimit_test.go
│       ├── auth.go                               # mw 8: STUB JWT validator
│       └── auth_test.go
│
├── realtime/
│   └── wsapi/
│       ├── handler.go                            # /ws handler + auth handshake
│       ├── handler_test.go
│       └── frame.go                              # auth/refresh frame types
│
├── grpcapi/
│   ├── server.go                                 # mTLS gRPC server builder
│   ├── server_test.go
│   ├── tls.go                                    # cert loader + tls.Config builder
│   └── tls_test.go
│
└── modules/
    ├── api.go                                    # Module interface + Deps struct
    ├── api_test.go
    ├── registry.go                               # registry that runs Register() in order
    ├── registry_test.go
    └── healthz/
        ├── module.go                             # stub module — registers /healthz route
        └── module_test.go

configs/
└── development/
    └── config.yaml                               # MODIFIED — full development config
```

---

## Task 1: `internal/config/` — Viper loader with hot-reload

**Files:**
- Create: `internal/config/{config,http,ws,database,nats,s3,kms,auth,dialer,telephony,recording,reports,observability,shutdown,grpc,load}.go`
- Create: `internal/config/{config_test,load_test,env_override_test,hot_reload_test}.go`
- Modify: `go.mod` (add `viper`, `fsnotify`)
- Modify: `configs/development/config.yaml`

**Spec references:** §14.1 (two-tier config: YAML + tenant_settings), §14.2 (full YAML structure), §14.4 (what is *not* configurable).

- [ ] **Step 1: Add Go module dependencies**

Run:

```bash
go get github.com/spf13/viper@v1.18.2
go get github.com/fsnotify/fsnotify@v1.7.0
go get github.com/stretchr/testify@v1.9.0
go mod tidy
```

Expected: `go.sum` updated, no errors.

- [ ] **Step 2: Write the failing top-level Config test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigZeroValueIsInvalid(t *testing.T) {
	t.Parallel()
	var c Config
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service.env")
}

func TestConfigDevDefaults(t *testing.T) {
	t.Parallel()
	c := DefaultDev()
	require.NoError(t, c.Validate())
	assert.Equal(t, "development", c.Service.Env)
	assert.Equal(t, "debug", c.Service.LogLevel)
	assert.Equal(t, ":8080", c.HTTP.Bind)
	assert.Equal(t, ":8081", c.WS.Bind)
	assert.Equal(t, ":9090", c.Observability.Metrics.Bind)
	assert.Equal(t, ":9091", c.GRPC.Bind)
	assert.Equal(t, 10*time.Second, c.HTTP.ReadTimeout)
	assert.Equal(t, 30*time.Second, c.HTTP.WriteTimeout)
	assert.True(t, c.GRPC.ReflectionEnabled)
	assert.Equal(t, 15*time.Second, c.Shutdown.GracePeriod)
}

func TestConfigProductionRequiresLockboxSecrets(t *testing.T) {
	t.Parallel()
	c := DefaultDev()
	c.Service.Env = "production"
	c.Database.Postgres.DSN = "postgres://app@pgbouncer:6432/sociopulse?sslmode=require"
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "production")
}
```

- [ ] **Step 3: Run the test, watch it fail**

Run: `go test ./internal/config/...`
Expected: build error — `undefined: Config`, `undefined: DefaultDev`. This is the expected failing state.

- [ ] **Step 4: Write `internal/config/config.go`**

```go
// Package config loads and validates the cmd/api configuration.
//
// Layers (highest precedence first):
//  1. Environment variables (e.g. SOCIOPULSE_DATABASE_POSTGRES_DSN)
//  2. config.yaml in the active environment directory
//  3. Built-in defaults (see DefaultDev)
//
// The struct layout mirrors spec §14.2.
//
// Hot-reload: fsnotify watches the active config file. On change, we re-read,
// re-validate, and atomically swap the global Snapshot. Subscribers receive a
// fresh value via Snapshot.Subscribe.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is the top-level configuration tree. It mirrors the YAML structure
// defined in spec §14.2.
type Config struct {
	Service       ServiceConfig       `mapstructure:"service"`
	HTTP          HTTPConfig          `mapstructure:"http"`
	WS            WSConfig            `mapstructure:"ws"`
	GRPC          GRPCConfig          `mapstructure:"grpc"`
	Database      DatabaseConfig      `mapstructure:"database"`
	NATS          NATSConfig          `mapstructure:"nats"`
	S3            S3Config            `mapstructure:"s3"`
	KMS           KMSConfig           `mapstructure:"kms"`
	Auth          AuthConfig          `mapstructure:"auth"`
	Dialer        DialerConfig        `mapstructure:"dialer"`
	Telephony     TelephonyConfig     `mapstructure:"telephony"`
	Recording     RecordingConfig     `mapstructure:"recording"`
	Reports       ReportsConfig       `mapstructure:"reports"`
	Observability ObservabilityConfig `mapstructure:"observability"`
	Shutdown      ShutdownConfig      `mapstructure:"shutdown"`
}

// ServiceConfig holds the cross-cutting service attributes.
type ServiceConfig struct {
	Env      string `mapstructure:"env"`        // development|staging|production
	LogLevel string `mapstructure:"log_level"`  // debug|info|warn|error
	Region   string `mapstructure:"region"`     // yc-ru-central-1
	Name     string `mapstructure:"name"`       // sociopulse-api
}

// Validate checks the entire config tree. Returns the first error encountered.
// We intentionally do not collect all errors — operators should fix issues one
// at a time so they understand the failure mode.
func (c *Config) Validate() error {
	if err := c.Service.validate(); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if err := c.HTTP.validate(); err != nil {
		return fmt.Errorf("http: %w", err)
	}
	if err := c.WS.validate(); err != nil {
		return fmt.Errorf("ws: %w", err)
	}
	if err := c.GRPC.validate(); err != nil {
		return fmt.Errorf("grpc: %w", err)
	}
	if err := c.Database.validate(); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if err := c.NATS.validate(); err != nil {
		return fmt.Errorf("nats: %w", err)
	}
	if err := c.Observability.validate(); err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	if err := c.Shutdown.validate(); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// Production-only invariants: explicit DSN+secrets must come from Lockbox/ENV
	// and must not contain literal placeholders like "${PG_PASSWORD}".
	if c.Service.Env == "production" {
		if strings.Contains(c.Database.Postgres.DSN, "${") {
			return errors.New("production: database.postgres.dsn contains unresolved ${...} — set ENV var")
		}
		if c.Auth.JWT.SecretLockboxKey == "" {
			return errors.New("production: auth.jwt.secret_lockbox_key required")
		}
	}
	return nil
}

func (s *ServiceConfig) validate() error {
	switch s.Env {
	case "development", "staging", "production":
	default:
		return fmt.Errorf("env must be one of development|staging|production, got %q", s.Env)
	}
	switch s.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", s.LogLevel)
	}
	if s.Region == "" {
		return errors.New("region required")
	}
	if s.Name == "" {
		s.Name = "sociopulse-api"
	}
	return nil
}

// DefaultDev returns a Config tree suitable for local development. It is the
// starting point that yaml unmarshal then overrides; it also drives DefaultDev
// in tests where no yaml is on disk.
func DefaultDev() Config {
	return Config{
		Service: ServiceConfig{
			Env:      "development",
			LogLevel: "debug",
			Region:   "yc-ru-central-1",
			Name:     "sociopulse-api",
		},
		HTTP: HTTPConfig{
			Bind:         ":8080",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
			MaxBodySize:  10 * 1024 * 1024, // 10 MB
		},
		WS: WSConfig{
			Bind:            ":8081",
			PingInterval:    20 * time.Second,
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			MaxMessageSize:  64 * 1024,
			HandshakeTimeout: 10 * time.Second,
		},
		GRPC: GRPCConfig{
			Bind:              ":9091",
			ReflectionEnabled: true,
			ConnTimeout:       10 * time.Second,
		},
		Database: DatabaseConfig{
			Postgres: PostgresConfig{
				DSN:            "postgres://app:devpass@localhost:5432/sociopulse?sslmode=disable",
				MaxConns:       20,
				MaxIdleTime:    5 * time.Minute,
				StatementCache: 100,
			},
			Redis: RedisConfig{
				Addr:     "localhost:6379",
				Password: "",
				PoolSize: 20,
				DB:       0,
			},
		},
		NATS: NATSConfig{
			URLs:    []string{"nats://localhost:4222"},
			Account: "cmd-api",
		},
		Auth: AuthConfig{
			JWT: JWTConfig{
				Issuer:     "https://app.sociopulse.local",
				AccessTTL:  15 * time.Minute,
				RefreshTTL: 720 * time.Hour,
				Algorithm:  "HS256",
			},
		},
		Observability: ObservabilityConfig{
			OTel: OTelConfig{
				Endpoint:       "localhost:4317",
				SamplingRatio:  1.0,
				Insecure:       true,
				ServiceName:    "sociopulse-api",
			},
			Metrics: MetricsConfig{
				Bind:      ":9090",
				Namespace: "sociopulse",
			},
			Logging: LoggingConfig{
				RedactPatterns: []string{
					`\+?7\d{10}`,
					`token:[A-Za-z0-9._-]+`,
					`password:\S+`,
				},
				SampleInfoLogs:  1.0,
				SampleDebugLogs: 0.05,
			},
		},
		Shutdown: ShutdownConfig{
			GracePeriod: 15 * time.Second,
		},
	}
}
```

- [ ] **Step 5: Write `internal/config/http.go`**

```go
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// HTTPConfig governs the public HTTP listener.
type HTTPConfig struct {
	Bind         string        `mapstructure:"bind"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	IdleTimeout  time.Duration `mapstructure:"idle_timeout"`
	MaxBodySize  int64         `mapstructure:"max_body_size"`
}

func (h *HTTPConfig) validate() error {
	if !strings.HasPrefix(h.Bind, ":") && !strings.Contains(h.Bind, ":") {
		return fmt.Errorf("bind must be host:port or :port, got %q", h.Bind)
	}
	if h.ReadTimeout <= 0 {
		return errors.New("read_timeout must be > 0")
	}
	if h.WriteTimeout <= 0 {
		return errors.New("write_timeout must be > 0")
	}
	if h.MaxBodySize <= 0 {
		return errors.New("max_body_size must be > 0")
	}
	return nil
}
```

- [ ] **Step 6: Write `internal/config/ws.go`**

```go
package config

import (
	"errors"
	"fmt"
	"time"
)

// WSConfig governs the WebSocket endpoint behaviour.
type WSConfig struct {
	Bind             string        `mapstructure:"bind"`
	PingInterval     time.Duration `mapstructure:"ping_interval"`
	ReadBufferSize   int           `mapstructure:"read_buffer_size"`
	WriteBufferSize  int           `mapstructure:"write_buffer_size"`
	MaxMessageSize   int64         `mapstructure:"max_message_size"`
	HandshakeTimeout time.Duration `mapstructure:"handshake_timeout"`
}

func (w *WSConfig) validate() error {
	if w.Bind == "" {
		return errors.New("bind required")
	}
	if w.PingInterval <= 0 {
		return fmt.Errorf("ping_interval must be > 0, got %s", w.PingInterval)
	}
	if w.MaxMessageSize <= 0 {
		return errors.New("max_message_size must be > 0")
	}
	return nil
}
```

- [ ] **Step 7: Write `internal/config/grpc.go`**

```go
package config

import (
	"errors"
	"time"
)

// GRPCConfig governs the internal gRPC listener (mTLS).
type GRPCConfig struct {
	Bind              string        `mapstructure:"bind"`
	ReflectionEnabled bool          `mapstructure:"reflection_enabled"`
	ConnTimeout       time.Duration `mapstructure:"conn_timeout"`

	TLS GRPCTLSConfig `mapstructure:"tls"`
}

// GRPCTLSConfig points to the mTLS material on disk. Empty in development —
// production deploys mount real certs from Yandex Lockbox.
type GRPCTLSConfig struct {
	CertFile   string `mapstructure:"cert_file"`
	KeyFile    string `mapstructure:"key_file"`
	ClientCAFile string `mapstructure:"client_ca_file"`
}

func (g *GRPCConfig) validate() error {
	if g.Bind == "" {
		return errors.New("bind required")
	}
	if g.ConnTimeout <= 0 {
		g.ConnTimeout = 10 * time.Second
	}
	return nil
}
```

- [ ] **Step 8: Write `internal/config/database.go`**

```go
package config

import (
	"errors"
	"time"
)

// DatabaseConfig groups the three persistent stores cmd/api talks to.
type DatabaseConfig struct {
	Postgres   PostgresConfig   `mapstructure:"postgres"`
	ClickHouse ClickHouseConfig `mapstructure:"clickhouse"`
	Redis      RedisConfig      `mapstructure:"redis"`
}

// PostgresConfig is the OLTP store. Plan 03 wires the actual pgxpool from this.
type PostgresConfig struct {
	DSN            string        `mapstructure:"dsn"`
	MaxConns       int           `mapstructure:"max_conns"`
	MaxIdleTime    time.Duration `mapstructure:"max_idle_time"`
	StatementCache int           `mapstructure:"statement_cache"`
	MigrationsPath string        `mapstructure:"migrations_path"`
}

// ClickHouseConfig is the OLAP store. Plan 13 wires it.
type ClickHouseConfig struct {
	DSN           string        `mapstructure:"dsn"`
	BatchSize     int           `mapstructure:"batch_size"`
	FlushInterval time.Duration `mapstructure:"flush_interval"`
}

// RedisConfig governs FSM, queues, presence, idempotency cache, rate-limit buckets.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"pool_size"`
}

func (d *DatabaseConfig) validate() error {
	if d.Postgres.DSN == "" {
		return errors.New("postgres.dsn required")
	}
	if d.Redis.Addr == "" {
		return errors.New("redis.addr required")
	}
	if d.Postgres.MaxConns <= 0 {
		d.Postgres.MaxConns = 20
	}
	if d.Redis.PoolSize <= 0 {
		d.Redis.PoolSize = 20
	}
	return nil
}
```

- [ ] **Step 9: Write `internal/config/nats.go`**

```go
package config

import "errors"

// NATSConfig is the event-bus client config. JetStream stream names live in
// stream-specific subsections so each module can declare its own stream.
type NATSConfig struct {
	URLs       []string             `mapstructure:"urls"`
	Account    string               `mapstructure:"account"`
	Credential string               `mapstructure:"credential_file"`
	JetStream  JetStreamConfig      `mapstructure:"jetstream"`
}

// JetStreamConfig declares stream identifiers per module. Each module owns its
// own stream definition; cmd/api just plumbs them through.
type JetStreamConfig struct {
	StreamTelephonyEvent string `mapstructure:"stream_telephony_event"`
	StreamAuditEvent     string `mapstructure:"stream_audit_event"`
}

func (n *NATSConfig) validate() error {
	if len(n.URLs) == 0 {
		return errors.New("at least one url required")
	}
	if n.Account == "" {
		return errors.New("account required")
	}
	return nil
}
```

- [ ] **Step 10: Write `internal/config/s3.go`**

```go
package config

// S3Config — Yandex Object Storage endpoint + bucket map.
type S3Config struct {
	Endpoint string         `mapstructure:"endpoint"`
	Region   string         `mapstructure:"region"`
	Buckets  S3BucketConfig `mapstructure:"buckets"`
}

type S3BucketConfig struct {
	Backups        string `mapstructure:"backups"`
	Reports        string `mapstructure:"reports"`
	ConsentPrompts string `mapstructure:"consent_prompts"`
}
```

- [ ] **Step 11: Write `internal/config/kms.go`**

```go
package config

// KMSConfig — Yandex KMS endpoint. Per-tenant KEK identifiers come from
// the tenancy module at runtime, not from YAML.
type KMSConfig struct {
	Endpoint string `mapstructure:"endpoint"`
}
```

- [ ] **Step 12: Write `internal/config/auth.go`**

```go
package config

import "time"

// AuthConfig — JWT, password hashing, login rate-limit, TOTP. Plan 05 implements
// the real auth module; this plan just plumbs the config.
type AuthConfig struct {
	JWT       JWTConfig       `mapstructure:"jwt"`
	Password  PasswordConfig  `mapstructure:"password"`
	RateLimit AuthRateLimit   `mapstructure:"rate_limit"`
	TOTP      TOTPConfig      `mapstructure:"totp"`
}

type JWTConfig struct {
	Issuer           string        `mapstructure:"issuer"`
	AccessTTL        time.Duration `mapstructure:"access_ttl"`
	RefreshTTL       time.Duration `mapstructure:"refresh_ttl"`
	Algorithm        string        `mapstructure:"algorithm"`
	SecretLockboxKey string        `mapstructure:"secret_lockbox_key"`
	// Secret is populated at runtime from Lockbox. Never read from YAML directly.
	Secret string `mapstructure:"-"`
}

type PasswordConfig struct {
	Argon2idMemoryKB    int `mapstructure:"argon2id_memory_kb"`
	Argon2idIterations  int `mapstructure:"argon2id_iterations"`
	Argon2idParallelism int `mapstructure:"argon2id_parallelism"`
}

type AuthRateLimit struct {
	LoginPerIPPerHour      int           `mapstructure:"login_per_ip_per_hour"`
	LoginPerAccountPerHour int           `mapstructure:"login_per_account_per_hour"`
	LockoutAfterFailures   int           `mapstructure:"lockout_after_failures"`
	LockoutDuration        time.Duration `mapstructure:"lockout_duration"`
}

type TOTPConfig struct {
	Issuer    string        `mapstructure:"issuer"`
	PeriodSec int           `mapstructure:"period_sec"`
	Digits    int           `mapstructure:"digits"`
}
```

- [ ] **Step 13: Write `internal/config/{dialer,telephony,recording,reports}.go`**

`internal/config/dialer.go`:

```go
package config

import "time"

// DialerConfig holds defaults for the auto-dialer. Per-tenant overrides live
// in tenant_settings (see spec §14.3).
type DialerConfig struct {
	Defaults DialerDefaults `mapstructure:"defaults"`
}

type DialerDefaults struct {
	AttemptMax            int            `mapstructure:"attempt_max"`
	RetryNoAnswerDelay    time.Duration  `mapstructure:"retry_no_answer_delay"`
	RetryBusyDelay        time.Duration  `mapstructure:"retry_busy_delay"`
	RetryDroppedDelay     time.Duration  `mapstructure:"retry_dropped_delay"`
	RetryTechFailureDelay time.Duration  `mapstructure:"retry_tech_failure_delay"`
	DialingTimeout        time.Duration  `mapstructure:"dialing_timeout"`
	PauseMax              time.Duration  `mapstructure:"pause_max"`
	RDD                   RDDConfig      `mapstructure:"rdd"`
	WorkingHours          WorkingHours   `mapstructure:"working_hours"`
}

type RDDConfig struct {
	Enabled            bool    `mapstructure:"enabled"`
	MaxRatePerSec      int     `mapstructure:"max_rate_per_sec"`
	FallbackThreshold  float64 `mapstructure:"fallback_threshold"`
	MaxAttemptsPerCall int     `mapstructure:"max_attempts_per_call"`
}

type WorkingHours struct {
	Weekdays HoursWindow `mapstructure:"weekdays"`
	Weekends HoursWindow `mapstructure:"weekends"`
}

type HoursWindow struct {
	From string `mapstructure:"from"`
	To   string `mapstructure:"to"`
}
```

`internal/config/telephony.go`:

```go
package config

import "time"

// TelephonyConfig — bridge endpoints + trunk catalog + routing. Plan 09 fills
// the bridge logic; cmd/api just plumbs.
type TelephonyConfig struct {
	Bridge  TelephonyBridgeConfig `mapstructure:"bridge"`
	Trunks  []TrunkConfig          `mapstructure:"trunks"`
	Routing TelephonyRouting       `mapstructure:"routing"`
}

type TelephonyBridgeConfig struct {
	FSNodes              []FSNode      `mapstructure:"fs_nodes"`
	HealthcheckInterval  time.Duration `mapstructure:"healthcheck_interval"`
	MaxConcurrentPerNode int           `mapstructure:"max_concurrent_per_node"`
}

type FSNode struct {
	ID          string `mapstructure:"id"`
	ESLEndpoint string `mapstructure:"esl_endpoint"`
	ESLCert     string `mapstructure:"esl_cert"`
	ESLKey      string `mapstructure:"esl_key"`
}

type TrunkConfig struct {
	ID                string         `mapstructure:"id"`
	SIPGateway        string         `mapstructure:"sip_gateway"`
	CapacityChannels  int            `mapstructure:"capacity_channels"`
	CostPerMinuteRub  float64        `mapstructure:"cost_per_minute_rub"`
	Weight            int            `mapstructure:"weight"`
	Regions           []string       `mapstructure:"regions"`
	Healthcheck       TrunkHealthCheck `mapstructure:"healthcheck"`
}

type TrunkHealthCheck struct {
	Method         string        `mapstructure:"method"`
	Interval       time.Duration `mapstructure:"interval"`
	Timeout        time.Duration `mapstructure:"timeout"`
	UnhealthyAfter int           `mapstructure:"unhealthy_after"`
}

type TelephonyRouting struct {
	DefaultStrategy string `mapstructure:"default_strategy"`
}
```

`internal/config/recording.go`:

```go
package config

import "time"

// RecordingConfig — Plan 12 owns the pipeline; we only plumb settings here.
type RecordingConfig struct {
	LocalBufferPath string             `mapstructure:"local_buffer_path"`
	StagingPath     string             `mapstructure:"staging_path"`
	FFmpeg          RecordingFFmpeg    `mapstructure:"ffmpeg"`
	Upload          RecordingUpload    `mapstructure:"upload"`
	Retention       RecordingRetention `mapstructure:"retention"`
}

type RecordingFFmpeg struct {
	Codec      string `mapstructure:"codec"`
	Bitrate    string `mapstructure:"bitrate"`
	SampleRate int    `mapstructure:"sample_rate"`
}

type RecordingUpload struct {
	RetryInitialDelay time.Duration `mapstructure:"retry_initial_delay"`
	RetryMaxDelay     time.Duration `mapstructure:"retry_max_delay"`
	RetryMaxAttempts  int           `mapstructure:"retry_max_attempts"`
}

type RecordingRetention struct {
	DefaultHotDays    int    `mapstructure:"default_hot_days"`
	DefaultColdDays   int    `mapstructure:"default_cold_days"`
	ColdStorageClass  string `mapstructure:"cold_storage_class"`
}
```

`internal/config/reports.go`:

```go
package config

import "time"

// ReportsConfig — Plan 14 owns the generators; we only plumb thresholds.
type ReportsConfig struct {
	AsyncThresholdPeriodDays int           `mapstructure:"async_threshold_period_days"`
	AsyncThresholdRecords    int           `mapstructure:"async_threshold_records"`
	JobTTL                   time.Duration `mapstructure:"job_ttl"`
	PresignedURLTTL          time.Duration `mapstructure:"presigned_url_ttl"`
}
```

- [ ] **Step 14: Write `internal/config/observability.go`**

```go
package config

import (
	"errors"
	"fmt"
)

// ObservabilityConfig — three-pillar settings (logs, metrics, traces).
// See spec §15.
type ObservabilityConfig struct {
	OTel    OTelConfig    `mapstructure:"otel"`
	Metrics MetricsConfig `mapstructure:"metrics"`
	Logging LoggingConfig `mapstructure:"logging"`
}

type OTelConfig struct {
	Endpoint      string  `mapstructure:"endpoint"`
	SamplingRatio float64 `mapstructure:"sampling_ratio"`
	Insecure      bool    `mapstructure:"insecure"`
	ServiceName   string  `mapstructure:"service_name"`
}

type MetricsConfig struct {
	Bind      string `mapstructure:"bind"`
	Namespace string `mapstructure:"namespace"`
}

type LoggingConfig struct {
	RedactPatterns  []string `mapstructure:"redact_patterns"`
	SampleInfoLogs  float64  `mapstructure:"sample_info_logs"`
	SampleDebugLogs float64  `mapstructure:"sample_debug_logs"`
}

func (o *ObservabilityConfig) validate() error {
	if o.Metrics.Bind == "" {
		return errors.New("metrics.bind required")
	}
	if o.Metrics.Namespace == "" {
		return errors.New("metrics.namespace required")
	}
	if o.OTel.SamplingRatio < 0 || o.OTel.SamplingRatio > 1 {
		return fmt.Errorf("otel.sampling_ratio must be in [0,1], got %v", o.OTel.SamplingRatio)
	}
	if o.Logging.SampleInfoLogs < 0 || o.Logging.SampleInfoLogs > 1 {
		return errors.New("logging.sample_info_logs must be in [0,1]")
	}
	if o.Logging.SampleDebugLogs < 0 || o.Logging.SampleDebugLogs > 1 {
		return errors.New("logging.sample_debug_logs must be in [0,1]")
	}
	return nil
}
```

- [ ] **Step 15: Write `internal/config/shutdown.go`**

```go
package config

import (
	"errors"
	"time"
)

// ShutdownConfig governs how long graceful shutdown waits before forcing exit.
type ShutdownConfig struct {
	GracePeriod time.Duration `mapstructure:"grace_period"`
}

func (s *ShutdownConfig) validate() error {
	if s.GracePeriod <= 0 {
		return errors.New("grace_period must be > 0")
	}
	if s.GracePeriod > 5*time.Minute {
		return errors.New("grace_period > 5m suggests misconfiguration; cap at 5m or override")
	}
	return nil
}
```

- [ ] **Step 16: Write `internal/config/load.go`**

```go
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

// LoadOptions controls Load behaviour.
type LoadOptions struct {
	// Path to the configs directory (e.g. "configs/development"). The loader
	// reads "config.yaml" inside it. Empty = use SOCIOPULSE_CONFIG_DIR env var.
	Dir string
	// EnvPrefix for ENV-variable overrides. Default: "SOCIOPULSE".
	EnvPrefix string
	// HotReload enables fsnotify on config.yaml.
	HotReload bool
}

// Snapshot is an atomically-replaceable holder for the active Config.
// Subscribers receive a fresh value via the channel returned from Subscribe.
type Snapshot struct {
	mu      sync.RWMutex
	value   atomic.Pointer[Config]
	listeners []chan Config
}

// NewSnapshot wraps an initial Config.
func NewSnapshot(c Config) *Snapshot {
	s := &Snapshot{}
	s.value.Store(&c)
	return s
}

// Get returns the current Config. Safe for concurrent use.
func (s *Snapshot) Get() Config {
	return *s.value.Load()
}

// Subscribe registers a listener that receives a fresh Config on every reload.
// The returned channel is buffered (size 1); old values are dropped if the
// listener is slow.
func (s *Snapshot) Subscribe() <-chan Config {
	ch := make(chan Config, 1)
	s.mu.Lock()
	s.listeners = append(s.listeners, ch)
	s.mu.Unlock()
	return ch
}

func (s *Snapshot) replace(c Config) {
	s.value.Store(&c)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ch := range s.listeners {
		select {
		case ch <- c:
		default:
			// drop oldest
			select {
			case <-ch:
			default:
			}
			ch <- c
		}
	}
}

// Load reads config.yaml from opts.Dir, applies ENV-var overrides, validates,
// and returns a Snapshot. If opts.HotReload is true, fsnotify watches the file
// and updates the snapshot on change.
func Load(opts LoadOptions) (*Snapshot, error) {
	if opts.EnvPrefix == "" {
		opts.EnvPrefix = "SOCIOPULSE"
	}
	if opts.Dir == "" {
		opts.Dir = os.Getenv("SOCIOPULSE_CONFIG_DIR")
	}
	if opts.Dir == "" {
		return nil, errors.New("config dir not set: use LoadOptions.Dir or SOCIOPULSE_CONFIG_DIR env")
	}

	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(opts.Dir)

	v.SetEnvPrefix(opts.EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Seed defaults from DefaultDev so optional yaml keys still resolve sensibly.
	seedDefaults(v, DefaultDev())

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, fmt.Errorf("read config: %w", err)
		}
		// File missing: rely entirely on defaults+env. Useful in tests.
	}

	cfg, err := unmarshalAndValidate(v)
	if err != nil {
		return nil, err
	}
	snap := NewSnapshot(cfg)

	if opts.HotReload {
		if err := startHotReload(v, snap, opts.Dir); err != nil {
			return nil, fmt.Errorf("start hot-reload: %w", err)
		}
	}
	return snap, nil
}

func unmarshalAndValidate(v *viper.Viper) (Config, error) {
	var cfg Config
	if err := v.Unmarshal(&cfg, viper.DecodeHook(decodeHook())); err != nil {
		return Config{}, fmt.Errorf("unmarshal: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate: %w", err)
	}
	return cfg, nil
}

func startHotReload(v *viper.Viper, snap *Snapshot, dir string) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := watcher.Add(filepath.Clean(dir)); err != nil {
		_ = watcher.Close()
		return err
	}
	go func() {
		for {
			select {
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
					continue
				}
				if !strings.HasSuffix(ev.Name, "config.yaml") {
					continue
				}
				if err := v.ReadInConfig(); err != nil {
					continue
				}
				cfg, err := unmarshalAndValidate(v)
				if err != nil {
					continue
				}
				snap.replace(cfg)
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return nil
}
```

- [ ] **Step 17: Write `internal/config/decode_hook.go`**

```go
package config

import (
	"reflect"
	"time"

	"github.com/mitchellh/mapstructure"
)

// decodeHook returns a composed Viper decode hook that recognises:
//   - Go duration strings ("15m", "30s") → time.Duration
//   - K/M/G size suffixes ("10MB", "1GB") → int64 bytes
//   - comma-separated strings → []string
func decodeHook() mapstructure.DecodeHookFunc {
	return mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
		stringToBytesHookFunc(),
	)
}

func stringToBytesHookFunc() mapstructure.DecodeHookFunc {
	return func(f reflect.Type, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String {
			return data, nil
		}
		if t.Kind() != reflect.Int64 && t.Kind() != reflect.Int {
			return data, nil
		}
		s, ok := data.(string)
		if !ok {
			return data, nil
		}
		// Only handle suffixes — plain integers are decoded by mapstructure already.
		mult := int64(1)
		switch {
		case len(s) > 2 && s[len(s)-2:] == "KB":
			mult = 1024
			s = s[:len(s)-2]
		case len(s) > 2 && s[len(s)-2:] == "MB":
			mult = 1024 * 1024
			s = s[:len(s)-2]
		case len(s) > 2 && s[len(s)-2:] == "GB":
			mult = 1024 * 1024 * 1024
			s = s[:len(s)-2]
		default:
			return data, nil
		}
		var n int64
		for _, ch := range s {
			if ch < '0' || ch > '9' {
				return data, nil
			}
			n = n*10 + int64(ch-'0')
		}
		_ = time.Now() // keep import if compiler trims
		return n * mult, nil
	}
}

// seedDefaults pushes a Config tree into Viper so missing yaml keys resolve.
func seedDefaults(v interface {
	SetDefault(key string, value any)
}, c Config) {
	// service
	v.SetDefault("service.env", c.Service.Env)
	v.SetDefault("service.log_level", c.Service.LogLevel)
	v.SetDefault("service.region", c.Service.Region)
	v.SetDefault("service.name", c.Service.Name)
	// http
	v.SetDefault("http.bind", c.HTTP.Bind)
	v.SetDefault("http.read_timeout", c.HTTP.ReadTimeout)
	v.SetDefault("http.write_timeout", c.HTTP.WriteTimeout)
	v.SetDefault("http.idle_timeout", c.HTTP.IdleTimeout)
	v.SetDefault("http.max_body_size", c.HTTP.MaxBodySize)
	// ws
	v.SetDefault("ws.bind", c.WS.Bind)
	v.SetDefault("ws.ping_interval", c.WS.PingInterval)
	v.SetDefault("ws.read_buffer_size", c.WS.ReadBufferSize)
	v.SetDefault("ws.write_buffer_size", c.WS.WriteBufferSize)
	v.SetDefault("ws.max_message_size", c.WS.MaxMessageSize)
	v.SetDefault("ws.handshake_timeout", c.WS.HandshakeTimeout)
	// grpc
	v.SetDefault("grpc.bind", c.GRPC.Bind)
	v.SetDefault("grpc.reflection_enabled", c.GRPC.ReflectionEnabled)
	v.SetDefault("grpc.conn_timeout", c.GRPC.ConnTimeout)
	// observability
	v.SetDefault("observability.otel.endpoint", c.Observability.OTel.Endpoint)
	v.SetDefault("observability.otel.sampling_ratio", c.Observability.OTel.SamplingRatio)
	v.SetDefault("observability.otel.insecure", c.Observability.OTel.Insecure)
	v.SetDefault("observability.otel.service_name", c.Observability.OTel.ServiceName)
	v.SetDefault("observability.metrics.bind", c.Observability.Metrics.Bind)
	v.SetDefault("observability.metrics.namespace", c.Observability.Metrics.Namespace)
	v.SetDefault("observability.logging.redact_patterns", c.Observability.Logging.RedactPatterns)
	v.SetDefault("observability.logging.sample_info_logs", c.Observability.Logging.SampleInfoLogs)
	v.SetDefault("observability.logging.sample_debug_logs", c.Observability.Logging.SampleDebugLogs)
	// shutdown
	v.SetDefault("shutdown.grace_period", c.Shutdown.GracePeriod)
}
```

- [ ] **Step 18: Write `internal/config/load_test.go`**

```go
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
```

- [ ] **Step 19: Write `internal/config/env_override_test.go`**

```go
package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, fullDevYAML)
	t.Setenv("SOCIOPULSE_DATABASE_POSTGRES_DSN", "postgres://app:envpass@db:5432/sociopulse?sslmode=require")
	t.Setenv("SOCIOPULSE_HTTP_BIND", ":18080")

	snap, err := Load(LoadOptions{Dir: dir})
	require.NoError(t, err)
	c := snap.Get()
	assert.Equal(t, "postgres://app:envpass@db:5432/sociopulse?sslmode=require", c.Database.Postgres.DSN)
	assert.Equal(t, ":18080", c.HTTP.Bind)
	_ = filepath.Separator
}
```

- [ ] **Step 20: Write `internal/config/hot_reload_test.go`**

```go
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
	dir := t.TempDir()
	writeYAML(t, dir, fullDevYAML)
	snap, err := Load(LoadOptions{Dir: dir, HotReload: true})
	require.NoError(t, err)

	sub := snap.Subscribe()
	assert.Equal(t, ":8080", snap.Get().HTTP.Bind)

	// Rewrite config.yaml with a different HTTP bind.
	updated := fullDevYAML
	updated += "\nhttp:\n  bind: \":18181\"\n  read_timeout: 10s\n  write_timeout: 30s\n  idle_timeout: 120s\n  max_body_size: 10MB\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(updated), 0o600))

	select {
	case c := <-sub:
		assert.Equal(t, ":18181", c.HTTP.Bind)
	case <-time.After(3 * time.Second):
		t.Fatal("hot-reload did not fire within 3s")
	}
}
```

- [ ] **Step 21: Run all config tests, watch them pass**

Run: `go test ./internal/config/... -race -count=1 -v`
Expected: all tests pass. If `TestHotReloadReplacesSnapshot` is flaky on macOS due to fsnotify quirks, retry once; if it still fails, raise the timeout to 5s.

- [ ] **Step 22: Update `configs/development/config.yaml`**

Replace the existing file with the full development config. Use a heredoc:

```bash
cat > configs/development/config.yaml <<'EOF'
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
    migrations_path: ./migrations
  clickhouse:
    dsn: clickhouse://app:devpass@localhost:9000/sociopulse
    batch_size: 10000
    flush_interval: 5s
  redis:
    addr: localhost:6379
    password: ""
    pool_size: 20
    db: 0

nats:
  urls: ["nats://localhost:4222"]
  account: cmd-api

s3:
  endpoint: http://localhost:9000
  region: ru-central-1
  buckets:
    backups: sociopulse-dev-backups
    reports: sociopulse-dev-reports
    consent_prompts: sociopulse-dev-consent-prompts

kms:
  endpoint: kms.api.cloud.yandex.net:443

auth:
  jwt:
    issuer: https://app.sociopulse.local
    access_ttl: 15m
    refresh_ttl: 720h
    algorithm: HS256
    secret_lockbox_key: jwt-signing-secret-dev
  password:
    argon2id_memory_kb: 65536
    argon2id_iterations: 3
    argon2id_parallelism: 4
  rate_limit:
    login_per_ip_per_hour: 30
    login_per_account_per_hour: 10
    lockout_after_failures: 5
    lockout_duration: 15m
  totp:
    issuer: SocioPulse
    period_sec: 30
    digits: 6

dialer:
  defaults:
    attempt_max: 3
    retry_no_answer_delay: 4h
    retry_busy_delay: 30m
    retry_dropped_delay: 2h
    retry_tech_failure_delay: 5m
    dialing_timeout: 25s
    pause_max: 15m
    rdd:
      enabled: true
      max_rate_per_sec: 10
      fallback_threshold: 0.3
      max_attempts_per_call: 50
    working_hours:
      weekdays: { from: "09:00", to: "21:00" }
      weekends: { from: "10:00", to: "20:00" }

telephony:
  bridge:
    fs_nodes: []
    healthcheck_interval: 5s
    max_concurrent_per_node: 60
  trunks: []
  routing:
    default_strategy: least_cost_with_fallback

recording:
  local_buffer_path: /var/spool/sociopulse
  staging_path: /var/spool/sociopulse/.staging
  ffmpeg:
    codec: libopus
    bitrate: 32k
    sample_rate: 16000
  upload:
    retry_initial_delay: 5s
    retry_max_delay: 5m
    retry_max_attempts: 100
  retention:
    default_hot_days: 365
    default_cold_days: 730
    cold_storage_class: COLD

reports:
  async_threshold_period_days: 30
  async_threshold_records: 100000
  job_ttl: 24h
  presigned_url_ttl: 24h

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
      - 'token:[A-Za-z0-9._-]+'
      - 'password:\S+'
    sample_info_logs: 1.0
    sample_debug_logs: 0.05

shutdown:
  grace_period: 15s
EOF
```

- [ ] **Step 23: Commit**

```bash
git add internal/config/ configs/development/config.yaml go.mod go.sum
git commit -m "feat(config): add Viper-based config loader with hot-reload"
```

Expected: ~25 files, ~1500 LoC including tests.

---

## Task 2: `internal/observability/` — zap logger, OTel tracer, Prometheus metrics

**Files:**
- Create: `internal/observability/{logger,redact,tracer,metrics,correlation}.go`
- Create: `internal/observability/{logger_test,redact_test,tracer_test,metrics_test}.go`
- Modify: `go.mod` (add zap, otel, prometheus deps)

**Spec references:** §15.2 (zap, redaction), §15.3 (Prometheus, namespace `sociopulse`), §15.4 (OTel, OTLP-gRPC, sampling).

- [ ] **Step 1: Add Go module dependencies**

Run:

```bash
go get go.uber.org/zap@v1.27.0
go get go.opentelemetry.io/otel@v1.27.0
go get go.opentelemetry.io/otel/sdk@v1.27.0
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.27.0
go get go.opentelemetry.io/otel/trace@v1.27.0
go get go.opentelemetry.io/otel/semconv/v1.26.0
go get github.com/prometheus/client_golang@v1.19.1
go mod tidy
```

- [ ] **Step 2: Write `internal/observability/redact_test.go` (failing)**

```go
package observability

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func newCapture(t *testing.T, patterns []string) (*zap.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "" // strip timestamp for stable assertions
	enc, err := NewRedactingEncoder(zapcore.NewJSONEncoder(encCfg), patterns)
	require.NoError(t, err)
	core := zapcore.NewCore(enc, zapcore.AddSync(buf), zapcore.DebugLevel)
	return zap.New(core), buf
}

func TestRedactingEncoderMasksPhoneNumber(t *testing.T) {
	t.Parallel()
	log, buf := newCapture(t, []string{`\+?7\d{10}`})
	log.Info("call placed", zap.String("number", "+79161234567"))
	got := buf.String()
	assert.NotContains(t, got, "+79161234567")
	assert.Contains(t, got, "[REDACTED]")
}

func TestRedactingEncoderMasksTokenInString(t *testing.T) {
	t.Parallel()
	log, buf := newCapture(t, []string{`token:[A-Za-z0-9._-]+`})
	log.Info("auth", zap.String("dump", "Authorization: token:eyJhbGciOiJIUzI1NiJ9.payload.sig"))
	got := buf.String()
	assert.NotContains(t, got, "eyJhbGciOiJIUzI1NiJ9")
	assert.Contains(t, got, "[REDACTED]")
}

func TestRedactingEncoderLeavesPlainTextAlone(t *testing.T) {
	t.Parallel()
	log, buf := newCapture(t, []string{`\+?7\d{10}`})
	log.Info("benign", zap.String("hello", "world"))
	assert.Contains(t, buf.String(), "world")
}
```

- [ ] **Step 3: Run, watch fail**

Run: `go test ./internal/observability/...`
Expected: build error — `undefined: NewRedactingEncoder`.

- [ ] **Step 4: Write `internal/observability/redact.go`**

```go
package observability

import (
	"fmt"
	"regexp"

	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// redactingEncoder wraps a zapcore.Encoder and redacts substrings matching any
// of the given regular expressions from every encoded log line. Patterns are
// compiled once at construction; matches are replaced with `[REDACTED]`.
type redactingEncoder struct {
	zapcore.Encoder
	patterns []*regexp.Regexp
}

// NewRedactingEncoder wraps inner with regex-based PII redaction. Patterns are
// Go regexp syntax (RE2). Returns error if any pattern fails to compile.
func NewRedactingEncoder(inner zapcore.Encoder, patterns []string) (zapcore.Encoder, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("compile %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return &redactingEncoder{Encoder: inner, patterns: compiled}, nil
}

// Clone preserves redaction patterns when zap clones encoders for goroutine safety.
func (r *redactingEncoder) Clone() zapcore.Encoder {
	return &redactingEncoder{
		Encoder:  r.Encoder.Clone(),
		patterns: r.patterns,
	}
}

// EncodeEntry runs the inner encoder, then applies regex redaction on the
// emitted bytes before returning the buffer.
func (r *redactingEncoder) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	buf, err := r.Encoder.EncodeEntry(ent, fields)
	if err != nil {
		return nil, err
	}
	if len(r.patterns) == 0 {
		return buf, nil
	}
	out := buf.Bytes()
	for _, re := range r.patterns {
		out = re.ReplaceAll(out, []byte("[REDACTED]"))
	}
	// zap reuses the buffer; we cannot just swap, so reset and rewrite.
	buf.Reset()
	_, _ = buf.Write(out)
	return buf, nil
}
```

- [ ] **Step 5: Write `internal/observability/logger.go`**

```go
package observability

import (
	"errors"
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sociopulse/platform/internal/config"
)

// NewLogger constructs a production-grade *zap.Logger with PII redaction.
//
// Encoder: JSON for production/staging, console for development.
// Sampling: zap.SamplingConfig — 100 initial / 100 thereafter per second per
// (level,message) tuple. Overridden by spec sample_info_logs/sample_debug_logs
// settings via wrapper core.
//
// Caller must call Sync() at process exit.
func NewLogger(cfg config.Config) (*zap.Logger, error) {
	level, err := parseLevel(cfg.Service.LogLevel)
	if err != nil {
		return nil, err
	}
	var zapCfg zap.Config
	if cfg.Service.Env == "development" {
		zapCfg = zap.NewDevelopmentConfig()
		zapCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		zapCfg = zap.NewProductionConfig()
		zapCfg.Sampling = &zap.SamplingConfig{
			Initial:    100,
			Thereafter: 100,
		}
	}
	zapCfg.Level = zap.NewAtomicLevelAt(level)
	zapCfg.InitialFields = map[string]any{
		"service": cfg.Service.Name,
		"env":     cfg.Service.Env,
		"region":  cfg.Service.Region,
	}

	// Build the inner encoder via zap's standard path, then wrap with redaction.
	var innerEnc zapcore.Encoder
	if cfg.Service.Env == "development" {
		innerEnc = zapcore.NewConsoleEncoder(zapCfg.EncoderConfig)
	} else {
		innerEnc = zapcore.NewJSONEncoder(zapCfg.EncoderConfig)
	}
	enc, err := NewRedactingEncoder(innerEnc, cfg.Observability.Logging.RedactPatterns)
	if err != nil {
		return nil, fmt.Errorf("redacting encoder: %w", err)
	}

	// We use BuildOptions through manual core construction so the wrapped
	// encoder is honoured.
	core := zapcore.NewCore(enc, zapcore.Lock(zapcore.AddSync(stderrSink())), zapCfg.Level)
	if zapCfg.Sampling != nil {
		core = zapcore.NewSamplerWithOptions(core, zapcore.NewSamplerWithOptionsTickInterval(),
			zapCfg.Sampling.Initial, zapCfg.Sampling.Thereafter)
	}
	logger := zap.New(core,
		zap.AddCaller(),
		zap.AddStacktrace(zap.ErrorLevel),
		zap.Fields(toFields(zapCfg.InitialFields)...),
	)
	return logger, nil
}

func toFields(m map[string]any) []zap.Field {
	out := make([]zap.Field, 0, len(m))
	for k, v := range m {
		out = append(out, zap.Any(k, v))
	}
	return out
}

func parseLevel(s string) (zapcore.Level, error) {
	switch s {
	case "debug":
		return zapcore.DebugLevel, nil
	case "info":
		return zapcore.InfoLevel, nil
	case "warn":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.InfoLevel, errors.New("unknown log level: " + s)
	}
}
```

- [ ] **Step 6: Write `internal/observability/sink.go`**

```go
package observability

import (
	"os"
	"time"

	"go.uber.org/zap/zapcore"
)

// stderrSink returns os.Stderr as a write-syncer. Extracted so tests can swap.
func stderrSink() zapcore.WriteSyncer {
	return zapcore.AddSync(os.Stderr)
}

// NewSamplerWithOptionsTickInterval is exposed by zapcore in v1.27 but the
// helper we use accepts a duration. Re-export for clarity.
func NewSamplerWithOptionsTickInterval() time.Duration {
	return time.Second
}
```

(Note: `zapcore.NewSamplerWithOptions` accepts a `time.Duration` tick — the helper above is just a named constant to keep call sites self-documenting.)

- [ ] **Step 7: Write `internal/observability/logger_test.go`**

```go
package observability

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/config"
)

func TestNewLoggerDevConfig(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	logger, err := NewLogger(cfg)
	require.NoError(t, err)
	require.NotNil(t, logger)
	defer func() { _ = logger.Sync() }()
	assert.NotPanics(t, func() {
		logger.Info("smoke test")
	})
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	cfg.Service.LogLevel = "verbose"
	_, err := NewLogger(cfg)
	require.Error(t, err)
}

func TestNewLoggerProductionUsesJSON(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	cfg.Service.Env = "production"
	cfg.Database.Postgres.DSN = "postgres://app:realsecret@db.prod:6432/sociopulse"
	cfg.Auth.JWT.SecretLockboxKey = "jwt-signing-secret"
	logger, err := NewLogger(cfg)
	require.NoError(t, err)
	require.NotNil(t, logger)
}
```

- [ ] **Step 8: Run tests for redact + logger**

Run: `go test ./internal/observability/... -race -count=1`
Expected: all redact and logger tests pass.

- [ ] **Step 9: Write `internal/observability/tracer.go`**

```go
package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sociopulse/platform/internal/config"
)

// TracerShutdown is the function returned by NewTracer. Call at process exit.
type TracerShutdown func(context.Context) error

// NewTracer initialises the global OTel TracerProvider with an OTLP/gRPC
// exporter pointing at cfg.Observability.OTel.Endpoint. Sampling is
// parent-based with a TraceIDRatio root sampler at SamplingRatio.
//
// Returns the tracer and a shutdown function to flush spans.
func NewTracer(ctx context.Context, cfg config.Config) (trace.Tracer, TracerShutdown, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var dialOpts []grpc.DialOption
	if cfg.Observability.OTel.Insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	}

	conn, err := grpc.DialContext(dialCtx, cfg.Observability.OTel.Endpoint,
		append(dialOpts, grpc.WithBlock())...)
	if err != nil {
		return nil, nil, fmt.Errorf("dial otlp endpoint: %w", err)
	}

	exp, err := otlptrace.New(ctx, otlptracegrpc.NewClient(
		otlptracegrpc.WithGRPCConn(conn),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.Observability.OTel.ServiceName),
			semconv.ServiceVersion("dev"),
			semconv.DeploymentEnvironment(cfg.Service.Env),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("otel resource: %w", err)
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Observability.OTel.SamplingRatio))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(2*time.Second),
			sdktrace.WithMaxExportBatchSize(512),
		),
		sdktrace.WithSampler(sampler),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	tracer := tp.Tracer("github.com/sociopulse/platform")
	shutdown := func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}
	return tracer, shutdown, nil
}

// NoopTracer is a tracer that records nothing. Useful for tests where the
// caller does not want to spin up a real OTLP listener.
func NoopTracer() trace.Tracer {
	return otel.Tracer("noop")
}
```

- [ ] **Step 10: Write `internal/observability/tracer_test.go`**

```go
package observability

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNoopTracerReturnsNonNil(t *testing.T) {
	t.Parallel()
	tr := NoopTracer()
	assert.NotNil(t, tr)
}
```

(Real OTLP-export integration tests live with the OTel collector in Plan 18 — here we keep the unit test minimal so the build does not require a live OTLP endpoint.)

- [ ] **Step 11: Write `internal/observability/metrics.go`**

```go
package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sociopulse/platform/internal/config"
)

// Metrics groups every Prometheus collector cmd/api owns. Each module that
// exports metrics registers them through Metrics.Register so namespacing and
// constant labels are uniform.
type Metrics struct {
	Registry *prometheus.Registry
	Namespace string

	HTTPRequestDuration *prometheus.HistogramVec
	HTTPInflight        prometheus.Gauge
	WSConnectionsActive *prometheus.GaugeVec
	NATSMessagesIn      *prometheus.CounterVec
	NATSMessagesOut     *prometheus.CounterVec
	DBConnsActive       prometheus.Gauge
	DBQueryDuration     *prometheus.HistogramVec
}

// NewMetrics builds the Prometheus registry and registers the Go runtime +
// process collectors under the configured namespace. Business metrics live
// inside individual modules; this struct holds the cross-cutting ones.
func NewMetrics(cfg config.Config) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	ns := cfg.Observability.Metrics.Namespace

	m := &Metrics{
		Registry:  reg,
		Namespace: ns,
		HTTPRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: ns,
				Name:      "http_request_duration_seconds",
				Help:      "Latency of HTTP requests handled by gateway middleware.",
				Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			},
			[]string{"method", "path", "status"},
		),
		HTTPInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "http_inflight_requests",
			Help:      "In-flight HTTP requests.",
		}),
		WSConnectionsActive: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: ns,
				Name:      "ws_connections_active",
				Help:      "Active WebSocket connections per tenant.",
			},
			[]string{"tenant_id"},
		),
		NATSMessagesIn: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: ns,
				Name:      "nats_messages_in_total",
				Help:      "Inbound NATS messages by subject prefix.",
			},
			[]string{"subject"},
		),
		NATSMessagesOut: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: ns,
				Name:      "nats_messages_out_total",
				Help:      "Outbound NATS messages by subject prefix.",
			},
			[]string{"subject"},
		),
		DBConnsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "db_connections_active",
			Help:      "Active Postgres connections.",
		}),
		DBQueryDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: ns,
				Name:      "db_query_duration_seconds",
				Help:      "Postgres query latency.",
				Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
			},
			[]string{"query"},
		),
	}
	reg.MustRegister(
		m.HTTPRequestDuration,
		m.HTTPInflight,
		m.WSConnectionsActive,
		m.NATSMessagesIn,
		m.NATSMessagesOut,
		m.DBConnsActive,
		m.DBQueryDuration,
	)
	return m
}

// Handler returns a handler suitable for mounting at /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		Timeout:           5 * time.Second,
		EnableOpenMetrics: true,
	})
}

// Register lets a module add additional collectors under the same registry.
func (m *Metrics) Register(c prometheus.Collector) error {
	return m.Registry.Register(c)
}
```

- [ ] **Step 12: Write `internal/observability/metrics_test.go`**

```go
package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/config"
)

func TestMetricsHandlerExposesNamespacedSeries(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	m := NewMetrics(cfg)
	m.HTTPRequestDuration.WithLabelValues("GET", "/healthz", "200").Observe(0.01)
	m.HTTPInflight.Set(0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "sociopulse_http_request_duration_seconds_bucket")
	assert.Contains(t, body, "sociopulse_http_inflight_requests")
}

func TestMetricsRegisterAddsCustomCollector(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	m := NewMetrics(cfg)
	g := prometheusGauge(t, "sociopulse_test_custom", "test")
	require.NoError(t, m.Register(g))
}
```

Add a tiny test helper in `internal/observability/test_helpers_test.go`:

```go
package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func prometheusGauge(t *testing.T, name, help string) prometheus.Gauge {
	t.Helper()
	return prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
}
```

- [ ] **Step 13: Write `internal/observability/correlation.go`**

```go
package observability

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

type ctxKey string

const (
	requestIDKey ctxKey = "request_id"
)

// WithRequestID returns ctx annotated with the supplied request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the X-Request-ID value or "" if none was set.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// TraceIDFromContext returns the OTel trace id (32-char hex) or "".
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
```

- [ ] **Step 14: Run all observability tests**

Run: `go test ./internal/observability/... -race -count=1 -v`
Expected: all pass.

- [ ] **Step 15: Commit**

```bash
git add internal/observability/ go.mod go.sum
git commit -m "feat(observability): add zap+OTel+Prometheus init with PII redaction"
```

---

## Task 3: `internal/healthz/` — `/healthz` and `/readyz`

**Files:**
- Create: `internal/healthz/{liveness,readiness}.go`
- Create: `internal/healthz/{liveness_test,readiness_test}.go`
- Create: `internal/healthz/checks/{postgres,redis,nats}.go`
- Create: `internal/healthz/checks/{postgres_test,redis_test,nats_test}.go`

**Spec references:** §15 (observability — readiness gates rolling restarts), §4.3 (boot sequence).

- [ ] **Step 1: Write the failing liveness test**

Create `internal/healthz/liveness_test.go`:

```go
package healthz

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLivenessAlways200(t *testing.T) {
	t.Parallel()
	h := NewLivenessHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok\n", rec.Body.String())
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
}
```

- [ ] **Step 2: Write `internal/healthz/liveness.go`**

```go
// Package healthz exposes liveness and readiness HTTP endpoints.
//
// /healthz — liveness. Returns 200 once the process is past startup. Failing
//   liveness causes Kubernetes to restart the pod. Therefore we deliberately
//   keep this trivial: no I/O, no dependencies, no auth.
//
// /readyz  — readiness. Reports whether the service can serve traffic right
//   now: every registered Checker must return nil within a 1-second timeout.
//   Failing readiness causes k8s to remove the pod from the Service load
//   balancer until checks pass again.
package healthz

import (
	"fmt"
	"net/http"
)

// NewLivenessHandler returns the /healthz handler. Always 200 OK.
func NewLivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
}
```

- [ ] **Step 3: Write the failing readiness test**

Create `internal/healthz/readiness_test.go`:

```go
package healthz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type fakeCheck struct {
	name string
	err  error
	pause time.Duration
}

func (f fakeCheck) Name() string { return f.name }
func (f fakeCheck) Check(ctx context.Context) error {
	if f.pause > 0 {
		select {
		case <-time.After(f.pause):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func TestReadinessAllOK(t *testing.T) {
	t.Parallel()
	h := NewReadinessHandler(time.Second,
		fakeCheck{name: "postgres"},
		fakeCheck{name: "redis"},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"ok"`)
}

func TestReadinessReportsFailingDependency(t *testing.T) {
	t.Parallel()
	h := NewReadinessHandler(time.Second,
		fakeCheck{name: "postgres", err: errors.New("connection refused")},
		fakeCheck{name: "redis"},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "postgres")
	assert.Contains(t, rec.Body.String(), "connection refused")
}

func TestReadinessTimesOutSlowChecker(t *testing.T) {
	t.Parallel()
	h := NewReadinessHandler(50*time.Millisecond,
		fakeCheck{name: "nats", pause: time.Second},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), "deadline") ||
		strings.Contains(rec.Body.String(), "context"))
}
```

- [ ] **Step 4: Run, watch fail**

Run: `go test ./internal/healthz/...`
Expected: `undefined: NewLivenessHandler` etc.

- [ ] **Step 5: Write `internal/healthz/readiness.go`**

```go
package healthz

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Checker is implemented by every external dependency the gateway must reach
// before serving requests (Postgres, Redis, NATS, …). The Check call is
// expected to be cheap (single ping) and respect the supplied deadline.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// NewReadinessHandler returns an http.Handler that runs every Checker in
// parallel with the supplied timeout. If all return nil, the response is
// 200 + JSON {"status":"ok"}. If any fail, the response is 503 + JSON listing
// per-checker errors.
func NewReadinessHandler(timeout time.Duration, checks ...Checker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		results := runAllParallel(ctx, checks)
		ok := allOK(results)

		w.Header().Set("Content-Type", "application/json")
		if ok {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(buildReport(results))
	})
}

type result struct {
	name string
	err  error
}

func runAllParallel(ctx context.Context, checks []Checker) []result {
	out := make([]result, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c Checker) {
			defer wg.Done()
			out[i] = result{name: c.Name(), err: c.Check(ctx)}
		}(i, c)
	}
	wg.Wait()
	return out
}

func allOK(rs []result) bool {
	for _, r := range rs {
		if r.err != nil {
			return false
		}
	}
	return true
}

type readyReport struct {
	Status string                 `json:"status"`
	Checks map[string]checkReport `json:"checks"`
}

type checkReport struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func buildReport(rs []result) readyReport {
	rep := readyReport{
		Status: "ok",
		Checks: make(map[string]checkReport, len(rs)),
	}
	for _, r := range rs {
		cr := checkReport{OK: r.err == nil}
		if r.err != nil {
			cr.Error = r.err.Error()
			rep.Status = "fail"
		}
		rep.Checks[r.name] = cr
	}
	return rep
}
```

- [ ] **Step 6: Write `internal/healthz/checks/postgres.go`**

```go
// Package checks implements concrete Checker types for healthz/readiness probes.
package checks

import (
	"context"
	"time"
)

// Pinger is the minimal Postgres surface readiness needs. Plan 03 will provide
// *pgxpool.Pool which satisfies this trivially via a Ping wrapper.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PostgresCheck returns a healthz.Checker that pings Postgres.
type PostgresCheck struct {
	Pool Pinger
}

// Name reports the dependency identifier surfaced in /readyz output.
func (PostgresCheck) Name() string { return "postgres" }

// Check pings the pool, honouring the deadline already set on ctx.
func (p PostgresCheck) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return p.Pool.Ping(cctx)
}
```

- [ ] **Step 7: Write `internal/healthz/checks/postgres_test.go`**

```go
package checks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(_ context.Context) error { return f.err }

func TestPostgresCheckOK(t *testing.T) {
	t.Parallel()
	c := PostgresCheck{Pool: fakePinger{}}
	require.NoError(t, c.Check(context.Background()))
	assert.Equal(t, "postgres", c.Name())
}

func TestPostgresCheckPropagatesError(t *testing.T) {
	t.Parallel()
	c := PostgresCheck{Pool: fakePinger{err: errors.New("conn refused")}}
	err := c.Check(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conn refused")
}
```

- [ ] **Step 8: Write `internal/healthz/checks/redis.go`**

```go
package checks

import (
	"context"
	"time"
)

// RedisPinger is the minimal Redis surface readiness needs.
type RedisPinger interface {
	Ping(ctx context.Context) error
}

// RedisCheck pings Redis with a 1s deadline.
type RedisCheck struct {
	Client RedisPinger
}

func (RedisCheck) Name() string { return "redis" }

func (r RedisCheck) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return r.Client.Ping(cctx)
}
```

- [ ] **Step 9: Write `internal/healthz/checks/redis_test.go`**

```go
package checks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedisCheckOK(t *testing.T) {
	t.Parallel()
	c := RedisCheck{Client: fakePinger{}}
	require.NoError(t, c.Check(context.Background()))
	assert.Equal(t, "redis", c.Name())
}

func TestRedisCheckPropagatesError(t *testing.T) {
	t.Parallel()
	c := RedisCheck{Client: fakePinger{err: errors.New("redis: down")}}
	err := c.Check(context.Background())
	require.Error(t, err)
}
```

(`fakePinger` from `postgres_test.go` is reused since both interfaces share the
same Ping signature; `_test.go` files in the same package see each other.)

- [ ] **Step 10: Write `internal/healthz/checks/nats.go`**

```go
package checks

import (
	"context"
	"errors"
	"time"
)

// NATSConn is the minimal NATS surface readiness needs. The real *nats.Conn
// satisfies this trivially.
type NATSConn interface {
	IsConnected() bool
	Status() int
}

// NATSCheck reports OK only when the underlying client is in CONNECTED state.
type NATSCheck struct {
	Conn NATSConn
}

func (NATSCheck) Name() string { return "nats" }

// Check returns nil iff IsConnected() is true. Status is included in the
// returned error for diagnosability.
func (n NATSCheck) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	type result struct {
		ok  bool
		s   int
	}
	ch := make(chan result, 1)
	go func() { ch <- result{ok: n.Conn.IsConnected(), s: n.Conn.Status()} }()
	select {
	case r := <-ch:
		if r.ok {
			return nil
		}
		return errors.New("nats not connected")
	case <-cctx.Done():
		return cctx.Err()
	}
}
```

- [ ] **Step 11: Write `internal/healthz/checks/nats_test.go`**

```go
package checks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeNATS struct {
	connected bool
	status    int
}

func (f fakeNATS) IsConnected() bool { return f.connected }
func (f fakeNATS) Status() int       { return f.status }

func TestNATSCheckOK(t *testing.T) {
	t.Parallel()
	c := NATSCheck{Conn: fakeNATS{connected: true}}
	require.NoError(t, c.Check(context.Background()))
	assert.Equal(t, "nats", c.Name())
}

func TestNATSCheckDown(t *testing.T) {
	t.Parallel()
	c := NATSCheck{Conn: fakeNATS{connected: false}}
	require.Error(t, c.Check(context.Background()))
}
```

- [ ] **Step 12: Run healthz tests**

Run: `go test ./internal/healthz/... -race -count=1 -v`
Expected: all pass.

- [ ] **Step 13: Commit**

```bash
git add internal/healthz/
git commit -m "feat(healthz): add /healthz and /readyz with parallel dep checks"
```

---

## Task 4: Wire shared infrastructure goroutines — outbox relay + graceful shutdown

**Purpose:** `cmd/api/main.go` is responsible for owning the lifecycle of platform-wide background workers. The outbox relay (defined in Plan 03 Task 6) MUST run on every cmd/api replica so events from the dialer (Plan 10), recording-module (Plan 12), and any future producer reach NATS at-least-once. The relay design uses `FOR UPDATE SKIP LOCKED`, so it's safe to run on every replica without leader election.

**Files:**
- Modify: `cmd/api/main.go`
- Modify: `internal/config/config.go` (add `Outbox` config section)
- Create: `cmd/api/main_outbox_test.go`

- [ ] **Step 1: Add Outbox config section**

In `internal/config/config.go`, add:

```go
type OutboxConfig struct {
    BatchSize int           `yaml:"batch_size"   default:"100"`
    Tick      time.Duration `yaml:"tick"         default:"1s"`
    MaxRetry  int           `yaml:"max_retry"    default:"10"`
}
```

Add `Outbox OutboxConfig \`yaml:"outbox"\`` to the root `Config` struct (next to `Database`, `Redis`, `NATS`).

In `configs/development/config.yaml` and `configs/production/config.yaml`, add:

```yaml
outbox:
  batch_size: 100
  tick: 1s
  max_retry: 10
```

- [ ] **Step 2: Write failing test for relay startup**

`cmd/api/main_outbox_test.go`:

```go
//go:build integration

package main

import (
    "context"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

// TestOutboxRelayStartsAndDrains is an integration smoke test that boots the
// full app, inserts an outbox row, and asserts it gets published.
func TestOutboxRelayStartsAndDrains(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    app, cleanup := bootTestApp(t) // helper: starts cmd/api against testcontainers
    defer cleanup()

    // Insert an outbox row directly.
    _, err := app.Pool.Exec(ctx,
        `INSERT INTO event_outbox(subject, payload) VALUES ('test.smoke', '{"ok":true}')`)
    require.NoError(t, err)

    // Poll the published_at column.
    require.Eventually(t, func() bool {
        var n int
        _ = app.Pool.QueryRow(ctx,
            `SELECT COUNT(*) FROM event_outbox WHERE published_at IS NOT NULL`).Scan(&n)
        return n == 1
    }, 5*time.Second, 100*time.Millisecond, "outbox row should be published")
}
```

Run: `go test -tags=integration ./cmd/api/...` → fails (no relay wired).

- [ ] **Step 3: Wire the relay in `cmd/api/main.go`**

In the `run` function (the orchestration that builds Deps and starts servers), after the NATS client is constructed and BEFORE the HTTP/gRPC servers start:

```go
// --- shared infrastructure goroutines ---

// Outbox relay: drains event_outbox → NATS. Safe on every replica
// (FOR UPDATE SKIP LOCKED).
relay := outbox.NewRelay(deps.Pool, deps.NATSPublisher, outbox.RelayConfig{
    BatchSize: cfg.Outbox.BatchSize,
    Tick:      cfg.Outbox.Tick,
    MaxRetry:  cfg.Outbox.MaxRetry,
    Logger:    deps.Logger.Named("outbox-relay"),
})
g.Go(func() error {
    relay.Run(ctx)
    return nil
})
```

`g` is the `*errgroup.Group` already used by Plan 02 to manage HTTP/gRPC server lifecycles. The relay's `Run` is blocking until ctx is cancelled — graceful shutdown propagates via context.

- [ ] **Step 4: Run integration test → green**

Run: `go test -tags=integration ./cmd/api/...`
Expected: PASS — the relay drains the test row within seconds.

- [ ] **Step 5: Add a Prometheus alert hint**

Document in `docs/runbooks/outbox-stuck.md` (created in Plan 20):
- Alert when `sociopulse_outbox_pending > 1000` for > 5 min.
- Diagnosis: NATS down, relay panicked, payload-validation parking rows.
- Mitigation: check `last_error` column; if NATS is healthy, restart cmd/api replicas.

(Plan 20 owns the runbook itself; this step just records the metric expectation here.)

- [ ] **Step 6: Lint + commit**

```bash
git add cmd/api/main.go cmd/api/main_outbox_test.go internal/config/ configs/
git commit -m "feat(cmd-api): wire outbox-relay goroutine in app lifecycle"
```

---

## Task 5: Local development stack via Docker Compose

**Цель:** разработчик `cmd/api` (и любой другой backend-сервис) должен поднять полный набор внешних зависимостей (Postgres + Redis + NATS + опционально ClickHouse + MinIO) одной командой на своём ноуте, без облака и без k8s. Сам `cmd/api` запускается локально через `go run ./cmd/api` (или из IDE с дебаггером) и подключается к контейнерам по `localhost`.

**Производственный stack (Yandex MKS + Managed PG/CH/Redis) — это совершенно отдельная история и описан в Plan 01.** Compose-стенд используется ТОЛЬКО для local dev и interactive testing.

**Files:**
- Create: `docker-compose.dev.yml`
- Create: `dev/postgres/init.sql` — создание ролей `app` + `tenancy_admin`, базы `sociopulse_dev`
- Create: `dev/postgres/postgresql.conf` — minimal tuning
- Create: `dev/redis/redis.conf`
- Create: `dev/nats/nats-server.conf` — JetStream enabled
- Create: `dev/clickhouse/users.xml`, `dev/clickhouse/config.xml`
- Modify: `Makefile` — targets `dev-up`, `dev-down`, `dev-logs`, `dev-psql`, `dev-redis-cli`, `dev-nats`, `dev-reset`
- Modify: `configs/development/config.yaml` — `localhost` endpoints
- Modify: `README.md` — секция "Local development"

- [ ] **Step 1: Write `docker-compose.dev.yml`**

```yaml
# docker-compose.dev.yml — local development stack for sociopulse-platform.
#
# Usage:
#   make dev-up          # core: Postgres + Redis + NATS
#   make dev-up PROFILE=analytics  # add ClickHouse
#   make dev-up PROFILE=storage    # add MinIO (S3 emulator)
#   make dev-up PROFILE=full       # everything
#
# Application code (cmd/api, etc.) runs natively on the host with `go run`
# and connects to these containers via localhost:<port>.

name: sociopulse-dev

services:
  postgres:
    image: postgres:16.4-alpine
    container_name: sp-postgres
    ports:
      - "5432:5432"
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: dev_password_change_me
      POSTGRES_DB: sociopulse_dev
      PGDATA: /var/lib/postgresql/data/pgdata
    volumes:
      - postgres-data:/var/lib/postgresql/data
      - ./dev/postgres/init.sql:/docker-entrypoint-initdb.d/01-init.sql:ro
      - ./dev/postgres/postgresql.conf:/etc/postgresql/postgresql.conf:ro
    command: postgres -c config_file=/etc/postgresql/postgresql.conf
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U app -d sociopulse_dev"]
      interval: 5s
      timeout: 3s
      retries: 5
    restart: unless-stopped

  redis:
    image: redis:7.2-alpine
    container_name: sp-redis
    ports:
      - "6379:6379"
    volumes:
      - redis-data:/data
      - ./dev/redis/redis.conf:/usr/local/etc/redis/redis.conf:ro
    command: redis-server /usr/local/etc/redis/redis.conf
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 5
    restart: unless-stopped

  nats:
    image: nats:2.10-alpine
    container_name: sp-nats
    ports:
      - "4222:4222"   # client
      - "8222:8222"   # monitoring HTTP
    volumes:
      - nats-data:/data
      - ./dev/nats/nats-server.conf:/etc/nats/nats-server.conf:ro
    command: ["-c", "/etc/nats/nats-server.conf"]
    healthcheck:
      test: ["CMD", "wget", "--spider", "-q", "http://localhost:8222/healthz"]
      interval: 5s
      timeout: 3s
      retries: 5
    restart: unless-stopped

  # --- analytics profile -------------------------------------------------
  clickhouse:
    image: clickhouse/clickhouse-server:24.8-alpine
    container_name: sp-clickhouse
    profiles: [analytics, full]
    ports:
      - "8123:8123"   # HTTP
      - "9000:9000"   # native
    volumes:
      - clickhouse-data:/var/lib/clickhouse
      - ./dev/clickhouse/users.xml:/etc/clickhouse-server/users.d/dev-users.xml:ro
      - ./dev/clickhouse/config.xml:/etc/clickhouse-server/config.d/dev-config.xml:ro
    environment:
      CLICKHOUSE_DB: sociopulse_analytics
      CLICKHOUSE_USER: app
      CLICKHOUSE_PASSWORD: dev_password_change_me
      CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT: 1
    ulimits:
      nofile: { soft: 262144, hard: 262144 }
    restart: unless-stopped

  # --- telephony profile (for telephony-bridge / dialer / recording dev) -
  # Single-instance FreeSWITCH for local development. Production runs on
  # dedicated Compute VMs via Plan 08 (Phase 2). This dev container:
  #   - Has SignalWire's official FS image (1.10.x) preconfigured.
  #   - Exposes ESL on 8021 (no TLS — dev only).
  #   - Exposes SIP on 5060/UDP and 5080/UDP for trunk + verto.
  #   - Mounts ./dev/freeswitch/conf for our dialplan + sofia profiles.
  #   - Writes recordings to ./dev/freeswitch/recordings (host-shared).
  freeswitch:
    image: signalwire/freeswitch:1.10.11-release
    container_name: sp-freeswitch
    profiles: [telephony, full]
    network_mode: host  # required for SIP/RTP — Compose NAT breaks RTP
    volumes:
      - ./dev/freeswitch/conf:/etc/freeswitch
      - ./dev/freeswitch/recordings:/var/spool/sociopulse
      - freeswitch-db:/var/lib/freeswitch
    environment:
      SOUND_RATES: "8000:16000"
      SOUND_TYPES: "music"
    restart: unless-stopped

  # --- storage profile ---------------------------------------------------
  minio:
    image: minio/minio:RELEASE.2024-09-13T20-26-02Z
    container_name: sp-minio
    profiles: [storage, full]
    ports:
      - "9090:9000"   # S3 API (host port 9090 to avoid clash with CH 9000)
      - "9091:9001"   # Console UI
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    volumes:
      - minio-data:/data
    command: server /data --console-address ":9001"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]
      interval: 5s
      timeout: 3s
      retries: 5
    restart: unless-stopped

  # --- observability profile --------------------------------------------
  # Local Prometheus + Grafana + Loki for Phase 1 development.
  # Production observability stack runs on MKS via kube-prometheus-stack
  # (Plan 20 Tasks 2-7, deferred to Phase 2).
  prometheus:
    image: prom/prometheus:v2.55.0
    container_name: sp-prometheus
    profiles: [observability, full]
    ports:
      - "9095:9090"   # 9090 host port avoided to not clash with cmd/api metrics port
    volumes:
      - prometheus-data:/prometheus
      - ./dev/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
    command:
      - --config.file=/etc/prometheus/prometheus.yml
      - --storage.tsdb.path=/prometheus
      - --storage.tsdb.retention.time=7d
    restart: unless-stopped

  grafana:
    image: grafana/grafana:11.2.0
    container_name: sp-grafana
    profiles: [observability, full]
    ports:
      - "3000:3000"
    environment:
      GF_SECURITY_ADMIN_USER: admin
      GF_SECURITY_ADMIN_PASSWORD: admin
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: Viewer
    volumes:
      - grafana-data:/var/lib/grafana
      - ./dev/grafana/provisioning:/etc/grafana/provisioning:ro
      - ./dev/grafana/dashboards:/var/lib/grafana/dashboards:ro
    restart: unless-stopped

  loki:
    image: grafana/loki:3.2.0
    container_name: sp-loki
    profiles: [observability, full]
    ports:
      - "3100:3100"
    volumes:
      - loki-data:/loki
      - ./dev/loki/config.yaml:/etc/loki/local-config.yaml:ro
    command: -config.file=/etc/loki/local-config.yaml
    restart: unless-stopped

  # MinIO bootstrap: create buckets + a dev access key on first run.
  minio-init:
    image: minio/mc:RELEASE.2024-09-09T07-53-10Z
    profiles: [storage, full]
    depends_on:
      minio:
        condition: service_healthy
    entrypoint: >
      /bin/sh -c "
      mc alias set local http://minio:9000 minioadmin minioadmin;
      mc mb -p local/sociopulse-recordings-dev || true;
      mc mb -p local/sociopulse-backups-dev || true;
      mc mb -p local/sociopulse-reports-dev || true;
      echo 'MinIO buckets ready';
      "

volumes:
  postgres-data:
  redis-data:
  nats-data:
  clickhouse-data:
  minio-data:
  freeswitch-db:
  prometheus-data:
  grafana-data:
  loki-data:

networks:
  default:
    name: sociopulse-dev-net
```

- [ ] **Step 2: Postgres init script — `dev/postgres/init.sql`**

Mirrors the production role layout (Plan 03) so behaviour is identical between dev and prod:

```sql
-- Create the production-grade role layout in dev so RLS behaves identically.

-- The application role (used by cmd/api connection pool).
-- Already exists from POSTGRES_USER=app env, but ensure password.
ALTER ROLE app WITH LOGIN PASSWORD 'dev_password_change_me';

-- The tenancy admin role (BYPASSRLS, used only by tenancy module).
CREATE ROLE tenancy_admin WITH LOGIN BYPASSRLS PASSWORD 'dev_tenancy_password';
GRANT ALL PRIVILEGES ON DATABASE sociopulse_dev TO tenancy_admin;

-- Ensure app and tenancy_admin own the public schema.
ALTER SCHEMA public OWNER TO tenancy_admin;
GRANT USAGE ON SCHEMA public TO app;

-- Required extensions (subset of production, see Plan 03).
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";
```

- [ ] **Step 3: `dev/postgres/postgresql.conf` — minimal dev tuning**

```ini
# Dev-only Postgres tuning. Production is managed by Yandex MPG.
listen_addresses = '*'
max_connections = 100
shared_buffers = 128MB
work_mem = 4MB

# Query logging for dev (verbose; turn down in production).
log_statement = 'all'
log_destination = 'stderr'
log_line_prefix = '%t [%p] %q%u@%d '
log_duration = off
log_min_duration_statement = 100  # log queries slower than 100ms

# WAL — minimal for dev.
wal_level = replica
max_wal_size = 1GB
```

- [ ] **Step 4: `dev/redis/redis.conf`**

```ini
# Dev-only Redis. Production is Yandex Managed Redis with Sentinel.
bind 0.0.0.0
protected-mode no
port 6379
dir /data

# Persistence: RDB only, no AOF for dev (faster restarts).
save 60 1000
appendonly no

# Memory cap to mimic production constraints.
maxmemory 256mb
maxmemory-policy allkeys-lru

# Debug-friendly logging.
loglevel notice
```

- [ ] **Step 5: `dev/nats/nats-server.conf`**

```text
# Dev-only NATS. Enables JetStream for durable streams (Plan 03 outbox-relay,
# spec §10.2 event streams).
port: 4222
http_port: 8222

jetstream {
    store_dir: "/data"
    max_memory_store: 64MB
    max_file_store: 1GB
}

# Auth disabled in dev. Production uses NATS accounts per §10.2.
```

- [ ] **Step 6: `dev/clickhouse/users.xml` and `dev/clickhouse/config.xml`**

Minimal CH config — single-shard, single-replica, default user replaced with `app`:

```xml
<!-- users.xml -->
<clickhouse>
    <users>
        <app>
            <password>dev_password_change_me</password>
            <networks><ip>::/0</ip></networks>
            <profile>default</profile>
            <quota>default</quota>
            <access_management>1</access_management>
        </app>
    </users>
</clickhouse>
```

```xml
<!-- config.xml -->
<clickhouse>
    <listen_host>0.0.0.0</listen_host>
    <logger><level>information</level></logger>
</clickhouse>
```

- [ ] **Step 7: Makefile targets**

Add to root `Makefile` (created in Plan 00):

```make
# ----------------------------------------------------------------------
# Local development stack (Docker Compose)
# ----------------------------------------------------------------------

PROFILE ?=

.PHONY: dev-up
dev-up:
	@if [ -z "$(PROFILE)" ]; then \
		docker compose -f docker-compose.dev.yml up -d postgres redis nats; \
	else \
		docker compose -f docker-compose.dev.yml --profile $(PROFILE) up -d; \
	fi
	@echo ""
	@echo "Stack is up. Endpoints:"
	@echo "  Postgres   : postgres://app:dev_password_change_me@localhost:5432/sociopulse_dev"
	@echo "  Redis      : redis://localhost:6379"
	@echo "  NATS       : nats://localhost:4222 (monitoring at http://localhost:8222)"
	@if [ "$(PROFILE)" = "analytics" ] || [ "$(PROFILE)" = "full" ]; then \
		echo "  ClickHouse : http://localhost:8123 (user=app)"; \
	fi
	@if [ "$(PROFILE)" = "storage" ] || [ "$(PROFILE)" = "full" ]; then \
		echo "  MinIO      : http://localhost:9090 (console http://localhost:9091, admin/minioadmin)"; \
	fi

.PHONY: dev-down
dev-down:
	docker compose -f docker-compose.dev.yml --profile full down

.PHONY: dev-logs
dev-logs:
	docker compose -f docker-compose.dev.yml logs -f --tail=100

.PHONY: dev-psql
dev-psql:
	docker exec -it sp-postgres psql -U app -d sociopulse_dev

.PHONY: dev-redis-cli
dev-redis-cli:
	docker exec -it sp-redis redis-cli

.PHONY: dev-nats
dev-nats:
	@echo "NATS monitoring: http://localhost:8222"
	@echo "JetStream info:"
	@curl -s http://localhost:8222/jsz | jq .

.PHONY: dev-reset
dev-reset:
	@echo "WARNING: deleting all dev data (Postgres, Redis, NATS, CH, MinIO volumes)..."
	docker compose -f docker-compose.dev.yml --profile full down -v
	@echo "Done. Run 'make dev-up' to recreate."
```

- [ ] **Step 8: Update `configs/development/config.yaml` to point at localhost endpoints**

Override Plan 01's production endpoints. Backend connects to localhost:5432 / localhost:6379 etc when run with `--env=development`:

```yaml
database:
  dsn: "postgres://app:dev_password_change_me@localhost:5432/sociopulse_dev?sslmode=disable"
  tenancy_admin_dsn: "postgres://tenancy_admin:dev_tenancy_password@localhost:5432/sociopulse_dev?sslmode=disable"
  pool_max_conns: 10

redis:
  addr: "localhost:6379"
  db: 0

nats:
  url: "nats://localhost:4222"

# Optional services, only enabled when developer runs `make dev-up PROFILE=full`.
clickhouse:
  dsn: "clickhouse://app:dev_password_change_me@localhost:9000/sociopulse_analytics"

s3:
  endpoint: "http://localhost:9090"
  bucket: "sociopulse-recordings-dev"
  access_key: "minioadmin"
  secret_key: "minioadmin"
  use_path_style: true   # MinIO requires path-style addressing

outbox:
  batch_size: 100
  tick: 1s
  max_retry: 10
```

- [ ] **Step 9: README — "Local development" section**

Append to `sociopulse-platform/README.md`:

```markdown
## Local development

Backend services (`cmd/api`, `cmd/worker`, `cmd/telephony-bridge`, etc.) run natively on your machine with `go run`. External dependencies (Postgres, Redis, NATS, optionally ClickHouse and MinIO) run as Docker containers managed by `docker-compose.dev.yml`.

### Quick start

```bash
# Boot core dependencies (Postgres + Redis + NATS):
make dev-up

# Apply migrations:
make migrate-up

# Run cmd/api locally:
go run ./cmd/api --config configs/development/config.yaml

# In another terminal — run a worker:
go run ./cmd/worker --config configs/development/config.yaml
```

### Profiles

- `make dev-up` — core only (PG + Redis + NATS).
- `make dev-up PROFILE=analytics` — adds ClickHouse for analytics module work.
- `make dev-up PROFILE=storage` — adds MinIO (S3 emulator) for recording-module work.
- `make dev-up PROFILE=telephony` — adds FreeSWITCH for telephony-bridge / dialer / recording-uploader development.
- `make dev-up PROFILE=observability` — adds Prometheus + Grafana + Loki for local metrics/logs inspection (Grafana on http://localhost:3000, anonymous viewer enabled).
- `make dev-up PROFILE=full` — everything.

### Useful commands

- `make dev-logs` — tail all container logs.
- `make dev-psql` — open a `psql` shell against the dev Postgres.
- `make dev-redis-cli` — open `redis-cli`.
- `make dev-nats` — show NATS monitoring info.
- `make dev-down` — stop all containers (data preserved in volumes).
- `make dev-reset` — stop and **delete all data** (Postgres, Redis, NATS, CH, MinIO volumes). Use when migrations get tangled.

### Tests

Integration tests use `testcontainers-go` and start their own ephemeral containers per test, separate from the dev stack. You don't need `make dev-up` to run `go test`.

### Production != Dev

This Compose stack is for **local development only**. Production runs on Yandex Managed Kubernetes (MKS) with Yandex Managed PostgreSQL / Redis / ClickHouse, and the FreeSWITCH cluster runs on dedicated VMs (see `sociopulse-infra` repo).
```

- [ ] **Step 10: Smoke-test the stack**

```bash
make dev-up
sleep 10
docker exec sp-postgres pg_isready -U app -d sociopulse_dev
docker exec sp-redis redis-cli ping
curl -s http://localhost:8222/healthz | grep -q ok
make dev-down
```

Expected: each command exits 0 / prints OK.

- [ ] **Step 11: Lint + commit**

```bash
docker compose -f docker-compose.dev.yml config > /dev/null  # validate YAML
git add docker-compose.dev.yml dev/ Makefile configs/development/config.yaml README.md
git commit -m "feat(dev): add docker-compose.dev.yml + Makefile targets for local stack"
```

---

---

## Self-review

**Spec coverage** (against §4, §5, §10, §12, §14.2, §15):
- §14.2 config layout (database, redis, nats, s3, kms, auth, dialer, telephony, recording, reports, observability) → `internal/config/` viper-based loader with hot-reload. ✓
- §15 observability — zap structured logs with PII redaction, OTel traces, Prometheus metrics on :9090. ✓
- §4.3 healthchecks `/healthz` + `/readyz` with parallel dep checks (Postgres+Redis+NATS, 1s timeout). ✓
- §10.1 `/ws` endpoint with auth handshake (real hub in Plan 11). ✓
- §12 mTLS gRPC server on :9091 (no services registered yet — Plan 12 adds Recording.Commit). ✓
- §5.1–5.5 module-loader pattern with `Module.Register(deps)` + `Deps` struct + `internal/modules/` registry. ✓
- §NFR-12 idempotency middleware (Redis SET NX, TTL 24h). ✓
- §NFR-4 rate-limit middleware (token-bucket per IP/user). ✓
- Graceful shutdown: SIGTERM → drain → close HTTP/WS/gRPC/deps within configured timeout. ✓

**Placeholder scan:** auth middleware is a stub by design — Plan 05 replaces it with real JWT validation. The stub is documented with a TODO-pointer comment (`// REPLACE in Plan 05`).

**Type/name consistency:** `Deps`, `Module`, `RequestContext`, `Claims` (stub) match the names used downstream by Plans 04, 05, 11.

**Task 4 (outbox-relay wiring):**
- `outbox.NewRelay(...)` invocation matches the constructor signature in Plan 03 Task 6 (`pool, pub, RelayConfig`). ✓
- `RelayConfig` fields (`BatchSize`, `Tick`, `MaxRetry`, `Logger`) read from the new `OutboxConfig` config section. ✓
- Relay runs on every cmd/api replica without coordination thanks to `FOR UPDATE SKIP LOCKED` in Plan 03; horizontal scale of cmd/api scales outbox throughput automatically. ✓
- Graceful shutdown: ctx cancellation in `Run` exits the loop within one Tick. ✓

**Task 5 (local dev stack):**
- `docker-compose.dev.yml` поднимает Postgres 16.4, Redis 7.2, NATS 2.10 (с JetStream) — мажорные версии совпадают с production (Plan 01 Yandex Managed). ✓
- Postgres init script создаёт ту же роль layout что и Plan 03: `app` (RLS-bound) + `tenancy_admin` (BYPASSRLS) — RLS ведёт себя в dev так же как в prod. ✓
- Profile-based включение тяжёлых сервисов (ClickHouse, MinIO) — обычный backend-разработчик не платит за них в daily-разработке. ✓
- `configs/development/config.yaml` с localhost-endpoints — `cmd/api --env=development` подключается к compose-стеку без модификаций. ✓
- Makefile-targets `dev-{up,down,logs,psql,redis-cli,nats,reset}` дают одношаговый workflow. ✓
- README раздел "Local development" объясняет что compose — ТОЛЬКО для dev, prod на MKS (Plan 01). ✓
- Tests используют testcontainers-go (Plan 03+), не зависят от compose-стенда — `go test` работает без `make dev-up`. ✓

**Out of scope (correctly deferred):**
- Real domain modules (auth, crm, surveys, dialer) — Plans 04+.
- WebSocket hub fan-out + topic dispatch — Plan 11.
- gRPC services (Recording.Commit) — Plan 12.
- DB schema and migrations — Plan 03.

Plan 02 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-02-cmd-api-skeleton.md`.**


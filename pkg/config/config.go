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
	Outbox        OutboxConfig        `mapstructure:"outbox"`
}

// ServiceConfig holds the cross-cutting service attributes.
type ServiceConfig struct {
	Env      string `mapstructure:"env"`       // development|staging|production
	LogLevel string `mapstructure:"log_level"` // debug|info|warn|error
	Region   string `mapstructure:"region"`    // yc-ru-central-1
	Name     string `mapstructure:"name"`      // sociopulse-api
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
	if err := c.Outbox.validate(); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	if err := c.KMS.Validate(); err != nil {
		return fmt.Errorf("kms: %w", err)
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
		if c.KMS.effective() == KMSProviderLocal {
			return errors.New("production: kms.provider must be \"yandex\"; the local provider is dev-only")
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
			Bind:             ":8081",
			PingInterval:     20 * time.Second,
			ReadBufferSize:   4096,
			WriteBufferSize:  4096,
			MaxMessageSize:   64 * 1024,
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
				Endpoint:      "localhost:4317",
				SamplingRatio: 1.0,
				Insecure:      true,
				ServiceName:   "sociopulse-api",
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
		Outbox: OutboxConfig{
			BatchSize: 100,
			Tick:      1 * time.Second,
			MaxRetry:  10,
		},
		KMS: KMSConfig{
			// Dev uses the in-process keychain so `make dev-up` boots
			// without a real Yandex KMS endpoint. The hex string below
			// is a fixed dev seed: 32 bytes of 0x42. Replace per-tenant
			// in tests via store.NewLocalKMSClient.
			Provider:    KMSProviderLocal,
			LocalKeyHex: "4242424242424242424242424242424242424242424242424242424242424242",
		},
	}
}

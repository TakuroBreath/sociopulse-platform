// Package config carries the typed Config struct that every binary
// loads at startup. It is the in-process projection of layers 3-5 of
// the configuration hierarchy described in
// docs/architecture/05-configuration.md (defaults → YAML →
// environment); per-tenant runtime overrides and Lockbox secrets enter
// through other paths.
//
// Each top-level binary slices Config into the sub-structs its modules
// need; modules consume their own typed config (e.g. auth.Config)
// rather than poking at viper.
//
// Real loader wiring (viper, env binding, validation) lands in Plan 02
// Task 1.
package config

import "time"

// Config is the project-wide configuration root. Sub-structs map to
// the YAML layout in docs/architecture/05-configuration.md and spec
// §14.2.
type Config struct {
	Service       ServiceConfig       `mapstructure:"service"`
	HTTP          HTTPConfig          `mapstructure:"http"`
	Database      DatabaseConfig      `mapstructure:"database"`
	Redis         RedisConfig         `mapstructure:"redis"`
	NATS          NATSConfig          `mapstructure:"nats"`
	S3            S3Config            `mapstructure:"s3"`
	KMS           KMSConfig           `mapstructure:"kms"`
	Auth          AuthConfig          `mapstructure:"auth"`
	Dialer        DialerConfig        `mapstructure:"dialer"`
	Telephony     TelephonyConfig     `mapstructure:"telephony"`
	Recording     RecordingConfig     `mapstructure:"recording"`
	Reports       ReportsConfig       `mapstructure:"reports"`
	Outbox        OutboxConfig        `mapstructure:"outbox"`
	Observability ObservabilityConfig `mapstructure:"observability"`
}

// ServiceConfig holds process-identity values used by every layer
// (logging fields, metric labels, span attributes).
type ServiceConfig struct {
	Name     string `mapstructure:"name"     validate:"required"`
	Env      string `mapstructure:"env"      validate:"oneof=development staging production"`
	LogLevel string `mapstructure:"log_level" validate:"oneof=debug info warn error"`
	Region   string `mapstructure:"region"`
	Version  string `mapstructure:"version"`
}

// HTTPConfig governs the public HTTP surface of cmd/api.
type HTTPConfig struct {
	Bind         string        `mapstructure:"bind"         validate:"hostname_port"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	MaxBodySize  string        `mapstructure:"max_body_size"`
}

// DatabaseConfig groups the persistent stores the platform connects to.
// ClickHouse lives here because it is logically a database even though
// the analytics module owns its access path.
type DatabaseConfig struct {
	Postgres   PostgresConfig   `mapstructure:"postgres"`
	ClickHouse ClickHouseConfig `mapstructure:"clickhouse"`
}

// PostgresConfig is consumed by pkg/postgres.New.
type PostgresConfig struct {
	DSN         string        `mapstructure:"dsn"          validate:"required"`
	MaxConns    int           `mapstructure:"max_conns"    validate:"min=1"`
	MaxIdleTime time.Duration `mapstructure:"max_idle_time"`
}

// ClickHouseConfig is consumed by the analytics module.
type ClickHouseConfig struct {
	DSN           string        `mapstructure:"dsn"            validate:"required"`
	BatchSize     int           `mapstructure:"batch_size"     validate:"min=1"`
	FlushInterval time.Duration `mapstructure:"flush_interval"`
}

// RedisConfig is consumed by every module that talks to Redis (queues,
// presence, idempotency keys).
type RedisConfig struct {
	Addr     string `mapstructure:"addr"      validate:"hostname_port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"pool_size" validate:"min=1"`
}

// NATSConfig is consumed by pkg/eventbus and pkg/outbox.Relay.
type NATSConfig struct {
	URLs    []string `mapstructure:"urls"     validate:"min=1,dive,required"`
	Account string   `mapstructure:"account"  validate:"required"`
}

// S3Config is consumed by the recording module.
type S3Config struct {
	Endpoint   string `mapstructure:"endpoint"`
	Region     string `mapstructure:"region"`
	BucketHot  string `mapstructure:"bucket_hot"`
	BucketCold string `mapstructure:"bucket_cold"`
	AccessKey  string `mapstructure:"access_key"`
	SecretKey  string `mapstructure:"secret_key"`
	UseSSL     bool   `mapstructure:"use_ssl"`
}

// KMSConfig is consumed by the encryption envelope wrappers.
type KMSConfig struct {
	Endpoint       string `mapstructure:"endpoint"`
	FolderID       string `mapstructure:"folder_id"`
	KeyID          string `mapstructure:"key_id"`
	ServiceAccount string `mapstructure:"service_account_key_path"`
}

// AuthConfig is consumed by the auth module.
type AuthConfig struct {
	JWTSecret           string        `mapstructure:"jwt_secret"            validate:"required"`
	AccessTokenTTL      time.Duration `mapstructure:"access_token_ttl"`
	RefreshTokenTTL     time.Duration `mapstructure:"refresh_token_ttl"`
	PasswordMinLength   int           `mapstructure:"password_min_length"   validate:"min=6"`
	Argon2Memory        uint32        `mapstructure:"argon2_memory_kib"`
	Argon2Iterations    uint32        `mapstructure:"argon2_iterations"`
	Argon2Parallelism   uint8         `mapstructure:"argon2_parallelism"`
	TOTPIssuer          string        `mapstructure:"totp_issuer"`
	RateLimitPerIP      int           `mapstructure:"rate_limit_per_ip"`
	RateLimitPerAccount int           `mapstructure:"rate_limit_per_account"`
}

// DialerConfig is consumed by the dialer module. Per-tenant overrides
// sit on top of these defaults via tenancy.SettingsCache.
type DialerConfig struct {
	Defaults DialerDefaults `mapstructure:"defaults"`
}

// DialerDefaults mirrors the per-tenant settings registry; the YAML
// supplies process-wide fallbacks.
type DialerDefaults struct {
	AttemptMax            int           `mapstructure:"attempt_max"`
	RetryNoAnswerDelay    time.Duration `mapstructure:"retry_no_answer_delay"`
	RetryBusyDelay        time.Duration `mapstructure:"retry_busy_delay"`
	RetryDroppedDelay     time.Duration `mapstructure:"retry_dropped_delay"`
	RetryTechFailureDelay time.Duration `mapstructure:"retry_tech_failure_delay"`
	DialingTimeout        time.Duration `mapstructure:"dialing_timeout"`
	PauseMax              time.Duration `mapstructure:"pause_max"`
	RDD                   RDDConfig     `mapstructure:"rdd"`
}

// RDDConfig governs random-digit-dialling generation.
type RDDConfig struct {
	Enabled       bool `mapstructure:"enabled"`
	MaxRatePerSec int  `mapstructure:"max_rate_per_sec"`
}

// TelephonyConfig is consumed by the telephony module + bridge.
type TelephonyConfig struct {
	BridgeAddr string        `mapstructure:"bridge_addr"`
	ESL        ESLConfig     `mapstructure:"esl"`
	Trunks     []TrunkConfig `mapstructure:"trunks"`
}

// ESLConfig is the FreeSWITCH ESL connection settings used by the
// telephony bridge.
type ESLConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Password string `mapstructure:"password"`
}

// TrunkConfig describes a SIP trunk; the dialer's least-cost router
// reads this list at startup.
type TrunkConfig struct {
	ID       string `mapstructure:"id"`
	Endpoint string `mapstructure:"endpoint"`
	Provider string `mapstructure:"provider"`
}

// RecordingConfig is consumed by the recording module + uploader.
type RecordingConfig struct {
	WatchDir              string        `mapstructure:"watch_dir"`
	UploadConcurrency     int           `mapstructure:"upload_concurrency"`
	HotRetentionDays      int           `mapstructure:"hot_retention_days"`
	ColdRetentionDays     int           `mapstructure:"cold_retention_days"`
	ConsentPromptURL      string        `mapstructure:"consent_prompt_url"`
	IntegrityCheckTimeout time.Duration `mapstructure:"integrity_check_timeout"`
}

// ReportsConfig is consumed by the reports module.
type ReportsConfig struct {
	WorkerConcurrency int           `mapstructure:"worker_concurrency"`
	MaxRows           int           `mapstructure:"max_rows"`
	RenderTimeout     time.Duration `mapstructure:"render_timeout"`
}

// OutboxConfig governs pkg/outbox.Relay.
type OutboxConfig struct {
	BatchSize      int           `mapstructure:"batch_size"`
	PollInterval   time.Duration `mapstructure:"poll_interval"`
	PublishTimeout time.Duration `mapstructure:"publish_timeout"`
}

// ObservabilityConfig is consumed by pkg/observability.
type ObservabilityConfig struct {
	Logging LoggingConfig `mapstructure:"logging"`
	Tracing TracingConfig `mapstructure:"tracing"`
	Metrics MetricsConfig `mapstructure:"metrics"`
}

// LoggingConfig configures the zap logger and PII redaction list.
type LoggingConfig struct {
	Encoding        string   `mapstructure:"encoding"           validate:"oneof=json console"`
	RedactPatterns  []string `mapstructure:"redact_patterns"`
	DebugSampleRate float64  `mapstructure:"debug_sample_rate"`
}

// TracingConfig configures the OTel tracer provider.
type TracingConfig struct {
	OTLPEndpoint  string  `mapstructure:"otlp_endpoint"`
	SamplingRatio float64 `mapstructure:"sampling_ratio" validate:"min=0,max=1"`
	Insecure      bool    `mapstructure:"insecure"`
}

// MetricsConfig configures the Prometheus exposition.
type MetricsConfig struct {
	Bind string `mapstructure:"bind" validate:"hostname_port"`
}

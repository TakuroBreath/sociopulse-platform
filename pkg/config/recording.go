package config

import "time"

// RecordingConfig — Plan 12 owns the pipeline; this struct plumbs
// settings into both the local recorder (cmd/recording-uploader, future)
// AND the cmd/api gRPC façade defined in Plan 12.1 Task 5.
//
// The RecordingService gRPC fields (Enabled, GRPCListenAddr, TLS*,
// MaxRecvBytes, Timeout) are consumed by internal/recording.Module —
// when Enabled=false the module skips the listener entirely and
// recording remains accessible via the in-process locator only. This
// is the default in dev/test where the cert material isn't
// provisioned.
type RecordingConfig struct {
	// Enabled gates the gRPC façade. False (default) means the
	// listener is not started; the in-process RecordingService is
	// still registered in the locator for HTTP transports.
	Enabled bool `mapstructure:"enabled"`

	// GRPCListenAddr is the host:port the gRPC server binds to.
	// Default ":9091" (mirrors GRPCConfig.Bind, but recording lives
	// on its own listener so cmd/api's existing gRPC config can be
	// repurposed for non-recording RPCs in the future).
	GRPCListenAddr string `mapstructure:"grpc_listen_addr"`

	// TLS material. mTLS is mandatory in production —
	// internal/recording/grpcserver requires all three to be set or
	// it refuses to construct the server. Empty paths disable the
	// listener with a WARN log, not a hard failure.
	TLSCertFile string `mapstructure:"tls_cert_file"`
	TLSKeyFile  string `mapstructure:"tls_key_file"`
	TLSCAFile   string `mapstructure:"tls_ca_file"`

	// MaxRecvBytes caps the per-message size; 0 falls back to the
	// gRPC server's default (4 MiB).
	MaxRecvBytes int `mapstructure:"max_recv_bytes"`

	// Timeout caps the per-call wall time; 0 falls back to the
	// server's default (30s).
	Timeout time.Duration `mapstructure:"timeout"`

	// Pipeline / uploader knobs. Owned by Plan 12 future tasks;
	// listed here so the YAML structure stays stable as the module
	// grows. Empty values are valid in dev.
	LocalBufferPath string             `mapstructure:"local_buffer_path"`
	StagingPath     string             `mapstructure:"staging_path"`
	FFmpeg          RecordingFFmpeg    `mapstructure:"ffmpeg"`
	Upload          RecordingUpload    `mapstructure:"upload"`
	Retention       RecordingRetention `mapstructure:"retention"`

	// Workers tunes the cmd/worker daemons that operate on call_recordings:
	// the retention pass (cold-move + hard-delete) and the integrity pass
	// (1% BERNOULLI sha256 verify). Both run only when Enabled=true AND
	// LocalKEKs validates — empty / invalid KEKs WARN + skip.
	//
	// Defaults match Plan 12.4 §8.5 + §9.4: 5min retention tick, 100-row
	// batch; 1h integrity tick, 10-row batch, 1% sample. Production
	// overrides only when ops dashboards justify a change.
	Workers RecordingWorkersConfig `mapstructure:"workers"`

	// LocalKEKs is a map of kms_key_id → hex-encoded 32-byte KEK plaintext.
	// Used by the Local DEKUnwrapper for dev/test environments without
	// access to Yandex Cloud KMS. Production deployments either build with
	// -tags=yandex_kms (which routes through the Yandex SDK adapter — Plan
	// 01) OR populate this with a platform-wide test KEK for non-prod
	// investigations. Keys are hex-encoded so config can be edited safely
	// in YAML / .env files.
	//
	// Format example (config.yaml):
	//
	//	recording:
	//	  local_keks:
	//	    kek-platform-test: "0000000000...000"  # 64 hex chars
	LocalKEKs map[string]string `mapstructure:"local_keks"`
}

// RecordingFFmpeg is the encoder configuration the local recorder hands to
// the ffmpeg process.
type RecordingFFmpeg struct {
	Codec      string `mapstructure:"codec"`
	Bitrate    string `mapstructure:"bitrate"`
	SampleRate int    `mapstructure:"sample_rate"`
}

// RecordingUpload tunes the retry behaviour when the uploader cannot reach
// object storage.
type RecordingUpload struct {
	RetryInitialDelay time.Duration `mapstructure:"retry_initial_delay"`
	RetryMaxDelay     time.Duration `mapstructure:"retry_max_delay"`
	RetryMaxAttempts  int           `mapstructure:"retry_max_attempts"`
}

// RecordingRetention controls hot/cold storage tier transitions.
type RecordingRetention struct {
	DefaultHotDays   int    `mapstructure:"default_hot_days"`
	DefaultColdDays  int    `mapstructure:"default_cold_days"`
	ColdStorageClass string `mapstructure:"cold_storage_class"`
}

// RecordingWorkersConfig wires cmd/worker's recording daemons.
//
// All five fields are nil-tolerant — zero values fall back to the
// in-package defaults (defaultRetentionInterval = 5m, etc.). The viper
// SetDefault calls in seed_defaults.go ensure operators see the
// intended values rather than relying on the worker package's
// implicit fallback.
type RecordingWorkersConfig struct {
	// RetentionInterval is the retention sweep cadence. 0 → 5m
	// (defaultRetentionInterval, set via seedDefaults).
	RetentionInterval time.Duration `mapstructure:"retention_interval"`

	// RetentionBatch caps the per-tick row count for cold-moves AND
	// deletes. 0 → 100.
	RetentionBatch int `mapstructure:"retention_batch"`

	// IntegrityInterval is the integrity sweep cadence. 0 → 1h.
	IntegrityInterval time.Duration `mapstructure:"integrity_interval"`

	// IntegrityBatch caps the per-tick row count fed to the BERNOULLI
	// sample. 0 → 10.
	IntegrityBatch int `mapstructure:"integrity_batch"`

	// IntegritySamplePercent is the BERNOULLI sample rate (0, 100].
	// 0 → 1.0 (1% — verifies a 7-day rotating window when paired with
	// the SQL-side `verified_at < now() - interval '7 days'` filter).
	IntegritySamplePercent float64 `mapstructure:"integrity_sample_percent"`
}

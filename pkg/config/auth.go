package config

import "time"

// AuthConfig — JWT, password hashing, login rate-limit, TOTP. Plan 05 implements
// the real auth module; this plan just plumbs the config.
type AuthConfig struct {
	JWT       JWTConfig      `mapstructure:"jwt"`
	Password  PasswordConfig `mapstructure:"password"`
	RateLimit AuthRateLimit  `mapstructure:"rate_limit"`
	TOTP      TOTPConfig     `mapstructure:"totp"`
}

// JWTConfig governs JSON Web Token issuance and verification. The signing
// secret itself is fetched from Lockbox at runtime — never read from YAML.
type JWTConfig struct {
	Issuer           string        `mapstructure:"issuer"`
	AccessTTL        time.Duration `mapstructure:"access_ttl"`
	RefreshTTL       time.Duration `mapstructure:"refresh_ttl"`
	Algorithm        string        `mapstructure:"algorithm"`
	SecretLockboxKey string        `mapstructure:"secret_lockbox_key"`
	// Secret is populated at runtime from Lockbox. Never read from YAML directly.
	Secret string `mapstructure:"-"`
}

// PasswordConfig holds Argon2id tuning parameters for password hashing.
type PasswordConfig struct {
	Argon2idMemoryKB    int `mapstructure:"argon2id_memory_kb"`
	Argon2idIterations  int `mapstructure:"argon2id_iterations"`
	Argon2idParallelism int `mapstructure:"argon2id_parallelism"`
}

// AuthRateLimit caps login attempts per IP, account, and the lockout window.
type AuthRateLimit struct {
	LoginPerIPPerHour      int           `mapstructure:"login_per_ip_per_hour"`
	LoginPerAccountPerHour int           `mapstructure:"login_per_account_per_hour"`
	LockoutAfterFailures   int           `mapstructure:"lockout_after_failures"`
	LockoutDuration        time.Duration `mapstructure:"lockout_duration"`
}

// TOTPConfig governs the TOTP second-factor (RFC 6238) parameters.
type TOTPConfig struct {
	Issuer    string `mapstructure:"issuer"`
	PeriodSec int    `mapstructure:"period_sec"`
	Digits    int    `mapstructure:"digits"`
}

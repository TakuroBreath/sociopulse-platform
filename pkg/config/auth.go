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

// JWTConfig governs JSON Web Token issuance and verification.
//
// Secret is the live signing key. Two binding paths are allowed:
//
//   - Production: the env var SOCIOPULSE_AUTH_JWT_SECRET is populated by
//     Kubernetes from a Lockbox-backed Secret resource (SecretLockboxKey
//     above names the Lockbox entry that the deployment binds). viper's
//     AutomaticEnv (pkg/config/load.go:126) reads it. Production
//     discipline says NEVER commit a real Secret to a checked-in YAML;
//     this is enforced by cfg.Validate() at config.go:96 which requires
//     SecretLockboxKey in env=production.
//   - Dev / smoke / unit-test: the YAML key `auth.jwt.secret` is the
//     bridge so cmd/api boots without external dependencies.
//
// Plan 21 Task 6 removed the previous `mapstructure:"-"` tag on Secret.
// The original intent ("populated at runtime from Lockbox") was correct
// in spirit but blocked auth.Module.Register under every in-process boot
// path because the field then had no way to acquire a non-empty value
// short of a hand-rolled post-unmarshal hook. Production hygiene is now
// enforced by Validate, not by the tag.
type JWTConfig struct {
	Issuer           string        `mapstructure:"issuer"`
	AccessTTL        time.Duration `mapstructure:"access_ttl"`
	RefreshTTL       time.Duration `mapstructure:"refresh_ttl"`
	Algorithm        string        `mapstructure:"algorithm"`
	SecretLockboxKey string        `mapstructure:"secret_lockbox_key"`
	Secret           string        `mapstructure:"secret"`
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

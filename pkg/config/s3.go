package config

import (
	"errors"
	"fmt"
)

// S3Provider names the implementation that backs BucketProvisioner.
//
// "yandex" — Yandex Object Storage, S3-compatible, gated behind the
// `yandex_s3` build tag like KMS for the same supply-chain reasons.
// "local"  — in-memory dev provisioner used by `make dev-up` so unit
// tests and local stacks do not need to talk to a real bucket service.
type S3Provider string

const (
	// S3ProviderYandex selects the Yandex Object Storage adapter.
	S3ProviderYandex S3Provider = "yandex"
	// S3ProviderLocal selects the in-memory dev adapter.
	S3ProviderLocal S3Provider = "local"
)

// S3Config — Object Storage endpoint + per-tenant bucket provisioning.
type S3Config struct {
	// Provider selects the BucketProvisioner implementation. Empty maps to
	// the local provider so `make dev-up` works without YAML edits. See
	// internal/tenancy/store for the implementations.
	Provider S3Provider `mapstructure:"provider"`

	// Endpoint is the S3-compatible API endpoint. For Yandex Object Storage
	// it is "storage.yandexcloud.net" (region-less) or a region-suffixed
	// equivalent. Required when Provider == S3ProviderYandex.
	Endpoint string `mapstructure:"endpoint"`

	// Region is the bucket region. Yandex Object Storage uses "ru-central1"
	// in production. Optional for the local provider.
	Region string `mapstructure:"region"`

	// AccessKeyID is the static access key for the production provider.
	// Mounted via Lockbox CSI in production; loaded once at module init.
	// Empty in dev/test.
	AccessKeyID string `mapstructure:"access_key_id"`

	// SecretAccessKey is the static secret key paired with AccessKeyID.
	// Mounted via Lockbox CSI; never logged.
	SecretAccessKey string `mapstructure:"secret_access_key"`

	// BucketPrefix is the deterministic prefix prepended to tenant IDs to
	// form a bucket name (default "sociopulse-recordings-"). Operators
	// override per-environment so dev/staging/prod buckets cannot collide.
	BucketPrefix string `mapstructure:"bucket_prefix"`

	// Buckets groups platform-shared bucket names cmd/api references at runtime.
	Buckets S3BucketConfig `mapstructure:"buckets"`
}

// S3BucketConfig groups the platform-shared bucket names cmd/api references
// at runtime. These are NOT per-tenant — they hold backups, reports
// definitions, and consent prompt media that span every tenant.
type S3BucketConfig struct {
	Backups        string `mapstructure:"backups"`
	Reports        string `mapstructure:"reports"`
	ConsentPrompts string `mapstructure:"consent_prompts"`
}

// effective returns the provider with the empty-string default resolved to
// "local". Kept private so callers always go through Validate().
func (c S3Config) effective() S3Provider {
	if c.Provider == "" {
		return S3ProviderLocal
	}
	return c.Provider
}

// EffectiveBucketPrefix returns the bucket prefix with the empty-string
// default resolved to "sociopulse-recordings-". The bucket name format is
// `<prefix><tenant_id>` so per-environment prefix overrides keep dev,
// staging, and prod buckets disjoint.
func (c S3Config) EffectiveBucketPrefix() string {
	if c.BucketPrefix == "" {
		return "sociopulse-recordings-"
	}
	return c.BucketPrefix
}

// Validate checks the per-provider invariants.
func (c S3Config) Validate() error {
	switch c.effective() {
	case S3ProviderLocal:
		// The in-memory dev provisioner has no required fields; bucket
		// names are derived from the deterministic prefix.
		return nil
	case S3ProviderYandex:
		if c.Endpoint == "" {
			return errors.New("s3: endpoint is required when provider is \"yandex\"")
		}
		if c.Region == "" {
			return errors.New("s3: region is required when provider is \"yandex\"")
		}
		if c.AccessKeyID == "" {
			return errors.New("s3: access_key_id is required when provider is \"yandex\"")
		}
		if c.SecretAccessKey == "" {
			return errors.New("s3: secret_access_key is required when provider is \"yandex\"")
		}
		return nil
	default:
		return fmt.Errorf("s3: unknown provider %q (want \"yandex\" or \"local\")", c.Provider)
	}
}

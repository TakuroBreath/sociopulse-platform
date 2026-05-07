package config

import (
	"errors"
	"fmt"
)

// KMSProvider names the backing Key Management Service.
//
// Two providers are supported in the platform today:
//   - "yandex" — production: per-tenant KEKs live in Yandex Cloud KMS;
//     the SDK adapter is compiled in only with `-tags=yandex_kms`.
//   - "local" — dev/test: an in-process AES-256-GCM keychain seeded
//     from `kms.local_key_hex`. Process restart drops every key.
//
// The empty string is treated as "local" so the zero-value Config and
// missing-yaml-key cases are ergonomic.
type KMSProvider string

const (
	// KMSProviderLocal selects the in-process keychain used by `make
	// dev-up` and unit tests.
	KMSProviderLocal KMSProvider = "local"

	// KMSProviderYandex selects the Yandex Cloud KMS adapter.
	KMSProviderYandex KMSProvider = "yandex"
)

// KMSConfig — KMS provider selection plus per-provider settings. Per-
// tenant KEK identifiers come from the tenancy module at runtime, not
// from YAML.
type KMSConfig struct {
	// Provider selects between "yandex" (production) and "local"
	// (dev/test). Empty defaults to "local".
	Provider KMSProvider `mapstructure:"provider"`

	// LocalKeyHex is the 32-byte hex-encoded master key used by the
	// in-process keychain (provider=="local"). Required when Provider
	// is "local". Ignored otherwise.
	LocalKeyHex string `mapstructure:"local_key_hex"`

	// Endpoint is the Yandex KMS gRPC endpoint
	// ("kms.api.cloud.yandex.net:443"). Required when Provider is
	// "yandex".
	Endpoint string `mapstructure:"endpoint"`

	// FolderID is the Yandex Cloud folder where per-tenant KEKs are
	// created. Required when Provider is "yandex".
	FolderID string `mapstructure:"folder_id"`

	// ServiceAccountKeyPath is the filesystem path to the IAM SA key
	// JSON used to authenticate to KMS. Required when Provider is
	// "yandex"; mounted by Lockbox CSI driver in production.
	ServiceAccountKeyPath string `mapstructure:"service_account_key_path"`
}

// effective returns the provider with the empty-string default
// resolved to "local". Kept private so callers always go through
// Validate().
func (c KMSConfig) effective() KMSProvider {
	if c.Provider == "" {
		return KMSProviderLocal
	}
	return c.Provider
}

// Validate checks the per-provider invariants and returns the first
// missing or invalid field.
func (c KMSConfig) Validate() error {
	switch c.effective() {
	case KMSProviderLocal:
		if c.LocalKeyHex == "" {
			return errors.New("kms: local_key_hex is required when provider is \"local\"")
		}
		// Length validation lives in store.NewLocalKMSClient — keep
		// the YAML-level check shallow so the failure surfaces early
		// (config load), with the deep crypto check at construction.
		return nil
	case KMSProviderYandex:
		if c.Endpoint == "" {
			return errors.New("kms: endpoint is required when provider is \"yandex\"")
		}
		if c.FolderID == "" {
			return errors.New("kms: folder_id is required when provider is \"yandex\"")
		}
		if c.ServiceAccountKeyPath == "" {
			return errors.New("kms: service_account_key_path is required when provider is \"yandex\"")
		}
		return nil
	default:
		return fmt.Errorf("kms: unknown provider %q (want \"yandex\" or \"local\")", c.Provider)
	}
}

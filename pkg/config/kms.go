package config

// KMSConfig — Yandex KMS endpoint. Per-tenant KEK identifiers come from
// the tenancy module at runtime, not from YAML.
type KMSConfig struct {
	Endpoint string `mapstructure:"endpoint"`
}

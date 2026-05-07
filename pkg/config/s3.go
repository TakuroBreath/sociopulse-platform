package config

// S3Config — Yandex Object Storage endpoint + bucket map.
type S3Config struct {
	Endpoint string         `mapstructure:"endpoint"`
	Region   string         `mapstructure:"region"`
	Buckets  S3BucketConfig `mapstructure:"buckets"`
}

// S3BucketConfig groups the bucket names cmd/api references at runtime.
type S3BucketConfig struct {
	Backups        string `mapstructure:"backups"`
	Reports        string `mapstructure:"reports"`
	ConsentPrompts string `mapstructure:"consent_prompts"`
}

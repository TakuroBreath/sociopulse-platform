package config

import "github.com/spf13/viper"

// Load reads configs/<env>/config.yaml plus the per-module fragments,
// overlays env vars (SOCIOPULSE_* prefix), and unmarshals everything
// into a typed Config. The full layering order is in
// docs/architecture/05-configuration.md.
//
// path is the configs/<env> directory or a single file; the loader
// disambiguates internally.
//
// Concrete viper wiring (env binding, file watching, struct
// validation) lands in Plan 02 Task 1.
func Load(path string) (*Config, error) {
	// Anchor the viper import so `go mod tidy` keeps it; the real
	// loader wiring lands in Plan 02 Task 1.
	_ = viper.New
	panic("not implemented: see Plan 02 Task 1")
}

// Watch enables hot-reload of non-secret keys (per
// docs/architecture/05-configuration.md § Hot-Reload). It panics today;
// the real implementation lands with Plan 02 Task 1.
func (c *Config) Watch(onChange func(*Config)) {
	panic("not implemented: see Plan 02 Task 1")
}

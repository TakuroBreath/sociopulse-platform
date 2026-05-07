package config

import (
	"errors"
	"time"
)

// GRPCConfig governs the internal gRPC listener (mTLS).
type GRPCConfig struct {
	Bind              string        `mapstructure:"bind"`
	ReflectionEnabled bool          `mapstructure:"reflection_enabled"`
	ConnTimeout       time.Duration `mapstructure:"conn_timeout"`

	TLS GRPCTLSConfig `mapstructure:"tls"`
}

// GRPCTLSConfig points to the mTLS material on disk. Empty in development —
// production deploys mount real certs from Yandex Lockbox.
type GRPCTLSConfig struct {
	CertFile     string `mapstructure:"cert_file"`
	KeyFile      string `mapstructure:"key_file"`
	ClientCAFile string `mapstructure:"client_ca_file"`
}

func (g *GRPCConfig) validate() error {
	if g.Bind == "" {
		return errors.New("bind required")
	}
	if g.ConnTimeout <= 0 {
		g.ConnTimeout = 10 * time.Second
	}
	return nil
}

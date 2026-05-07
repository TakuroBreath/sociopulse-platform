package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// HTTPConfig governs the public HTTP listener.
type HTTPConfig struct {
	Bind         string        `mapstructure:"bind"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	IdleTimeout  time.Duration `mapstructure:"idle_timeout"`
	MaxBodySize  int64         `mapstructure:"max_body_size"`
}

func (h *HTTPConfig) validate() error {
	if !strings.HasPrefix(h.Bind, ":") && !strings.Contains(h.Bind, ":") {
		return fmt.Errorf("bind must be host:port or :port, got %q", h.Bind)
	}
	if h.ReadTimeout <= 0 {
		return errors.New("read_timeout must be > 0")
	}
	if h.WriteTimeout <= 0 {
		return errors.New("write_timeout must be > 0")
	}
	if h.MaxBodySize <= 0 {
		return errors.New("max_body_size must be > 0")
	}
	return nil
}

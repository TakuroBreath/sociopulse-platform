package config

import (
	"errors"
	"time"
)

// ShutdownConfig governs how long graceful shutdown waits before forcing exit.
type ShutdownConfig struct {
	GracePeriod time.Duration `mapstructure:"grace_period"`
}

func (s *ShutdownConfig) validate() error {
	if s.GracePeriod <= 0 {
		return errors.New("grace_period must be > 0")
	}
	if s.GracePeriod > 5*time.Minute {
		return errors.New("grace_period > 5m suggests misconfiguration; cap at 5m or override")
	}
	return nil
}

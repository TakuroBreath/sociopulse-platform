package config

import (
	"errors"
	"fmt"
	"time"
)

// WSConfig governs the WebSocket endpoint behaviour.
type WSConfig struct {
	Bind             string        `mapstructure:"bind"`
	PingInterval     time.Duration `mapstructure:"ping_interval"`
	ReadBufferSize   int           `mapstructure:"read_buffer_size"`
	WriteBufferSize  int           `mapstructure:"write_buffer_size"`
	MaxMessageSize   int64         `mapstructure:"max_message_size"`
	HandshakeTimeout time.Duration `mapstructure:"handshake_timeout"`
}

func (w *WSConfig) validate() error {
	if w.Bind == "" {
		return errors.New("bind required")
	}
	if w.PingInterval <= 0 {
		return fmt.Errorf("ping_interval must be > 0, got %s", w.PingInterval)
	}
	if w.MaxMessageSize <= 0 {
		return errors.New("max_message_size must be > 0")
	}
	return nil
}

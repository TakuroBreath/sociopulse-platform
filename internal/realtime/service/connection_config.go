package service

import (
	"time"

	"go.uber.org/zap"
)

// ConnectionConfig tunes the per-connection lifecycle.
//
// Zero values pick production defaults documented per-field; tests may
// override individual knobs. The repo-wide WebSocket baseline (matches
// the Plan 10 dialer transport) is 30s ping period / 60s pong grace /
// 5s write timeout.
type ConnectionConfig struct {
	// AuthTimeout bounds AuthHandshake. The first inbound frame must
	// arrive within this window or the connection is dropped without
	// spawning the reader/writer/pinger goroutines. Zero -> 5s.
	AuthTimeout time.Duration

	// PingPeriod is the interval between server-initiated pings.
	// Zero -> 30s.
	PingPeriod time.Duration

	// PongTimeout bounds how long the pinger waits between observed
	// pongs before declaring the connection dead. Zero -> 60s.
	//
	// Drop-on-no-pong fires at PingPeriod + PongTimeout (= 90s) by
	// construction: the pinger checks lastPongAt every PingPeriod and
	// requires drift < PongTimeout.
	PongTimeout time.Duration

	// WriteTimeout bounds a single Frame write. Zero -> 5s.
	WriteTimeout time.Duration

	// WriteBufferSize is the per-connection telemetryCh capacity. A
	// full buffer triggers drop-oldest replacement on the next Send
	// for FrameClassTelemetry frames. Critical-class frames go on a
	// separate fixed-size queue (criticalQueueSize) and overflow to a
	// connection close. Zero -> 256.
	WriteBufferSize int

	// ReadFrameLimit caps inbound frame size in bytes. Zero -> 65 536.
	// (coder/websocket enforces via SetReadLimit; the field is plumbed
	// through ConnectionConfig so the HTTP handler can adjust the
	// underlying *websocket.Conn before constructing Connection.)
	ReadFrameLimit int64

	// RateLimitPerSec is the inbound frame rate-limit (token bucket).
	// Zero -> 100 frames/sec. A reader that exceeds the limit is
	// closed with CloseRateLimited.
	RateLimitPerSec int

	// RateLimitBurst is the token-bucket burst capacity (max accumulated
	// tokens). Zero -> RateLimitPerSec.
	RateLimitBurst int

	// Logger is the structured logger. Zero -> zap.NewNop().
	Logger *zap.Logger

	// Clock is the time source for all internal scheduling — pongs,
	// ticker creation, rate-limit refills. Zero -> time.Now. Tests
	// inject a controllable clock via NewConnectionWithClock.
	Clock func() time.Time
}

// defaults fills in zero-valued fields with the production defaults.
// Idempotent: calling on a fully-populated config is a no-op.
func (c *ConnectionConfig) defaults() {
	if c.AuthTimeout <= 0 {
		c.AuthTimeout = 5 * time.Second
	}
	if c.PingPeriod <= 0 {
		c.PingPeriod = 30 * time.Second
	}
	if c.PongTimeout <= 0 {
		c.PongTimeout = 60 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 5 * time.Second
	}
	if c.WriteBufferSize <= 0 {
		c.WriteBufferSize = 256
	}
	if c.ReadFrameLimit <= 0 {
		c.ReadFrameLimit = 64 * 1024
	}
	if c.RateLimitPerSec <= 0 {
		c.RateLimitPerSec = 100
	}
	if c.RateLimitBurst <= 0 {
		c.RateLimitBurst = c.RateLimitPerSec
	}
	if c.Logger == nil {
		c.Logger = zap.NewNop()
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

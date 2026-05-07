package observability

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sociopulse/platform/pkg/config"
)

func TestNewLoggerDevConfig(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	logger, err := NewLogger(cfg)
	require.NoError(t, err)
	require.NotNil(t, logger)
	defer func() { _ = logger.Sync() }()
	assert.NotPanics(t, func() {
		logger.Info("smoke test")
	})
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	cfg.Service.LogLevel = "verbose"
	_, err := NewLogger(cfg)
	require.Error(t, err)
}

func TestNewLoggerProductionConfig(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	cfg.Service.Env = "production"
	logger, err := NewLogger(cfg)
	require.NoError(t, err)
	require.NotNil(t, logger)
}

func TestNewLoggerRejectsBadRedactPattern(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	cfg.Observability.Logging.RedactPatterns = []string{`(unclosed`}
	_, err := NewLogger(cfg)
	require.Error(t, err)
}

// TestNewLoggerWithSinkRedactsPII verifies the end-to-end logger pipeline:
// the redacting encoder is wired in and active in the production logger.
func TestNewLoggerWithSinkRedactsPII(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	cfg := config.DefaultDev()
	cfg.Service.Env = "production"
	logger, err := newLoggerWithSink(cfg, zapcore.AddSync(buf))
	require.NoError(t, err)

	logger.Info("call placed", zap.String("number", "+79161234567"))
	require.NoError(t, logger.Sync())

	got := buf.String()
	assert.NotContains(t, got, "+79161234567")
	assert.Contains(t, got, "[REDACTED]")
}

// TestNewLoggerInitialFieldsAttached verifies that service/env/region fields
// are attached to every log line.
func TestNewLoggerInitialFieldsAttached(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	cfg := config.DefaultDev()
	cfg.Service.Env = "production"
	cfg.Service.Name = "sociopulse-api"
	cfg.Service.Region = "yc-ru-central-1"
	logger, err := newLoggerWithSink(cfg, zapcore.AddSync(buf))
	require.NoError(t, err)

	logger.Info("hello")
	require.NoError(t, logger.Sync())

	got := buf.String()
	assert.Contains(t, got, "sociopulse-api")
	assert.Contains(t, got, "yc-ru-central-1")
	assert.Contains(t, got, "production")
}

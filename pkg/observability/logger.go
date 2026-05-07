// Package observability is the project-wide telemetry toolkit: the zap
// logger factory, the OTel tracer/meter constructors, and the gin
// middleware that ties them together at the HTTP edge.
//
// Modules pull a *zap.Logger out of their Deps and a tracer/meter from
// the global OTel providers; they never construct telemetry primitives
// directly. The contract surfaces, log fields, metric names, span
// names, and PII redaction rules are documented in
// docs/architecture/06-observability.md.
package observability

import (
	"errors"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sociopulse/platform/pkg/config"
)

// NewLogger constructs a production-grade *zap.Logger with PII redaction.
//
// Encoder: JSON for production/staging, console for development. Sampling for
// non-development environments uses a fixed bucket of 100 initial / 100
// thereafter per second per (level,message) tuple, preventing a hot loop from
// drowning the log pipeline.
//
// The returned logger has the redaction encoder layered over zap's own JSON
// (or console) encoder; patterns come from cfg.Observability.Logging.
//
// Caller must call Sync() at process exit.
func NewLogger(cfg config.Config) (*zap.Logger, error) {
	return newLoggerWithSink(cfg, zapcore.Lock(zapcore.AddSync(os.Stderr)))
}

// newLoggerWithSink is the testable form of NewLogger that accepts an explicit
// write syncer. Production callers go through NewLogger.
func newLoggerWithSink(cfg config.Config, sink zapcore.WriteSyncer) (*zap.Logger, error) {
	level, err := parseLevel(cfg.Service.LogLevel)
	if err != nil {
		return nil, err
	}

	var encCfg zapcore.EncoderConfig
	var innerEnc zapcore.Encoder
	var sampling *zap.SamplingConfig
	if cfg.Service.Env == "development" {
		encCfg = zap.NewDevelopmentEncoderConfig()
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		innerEnc = zapcore.NewConsoleEncoder(encCfg)
	} else {
		encCfg = zap.NewProductionEncoderConfig()
		innerEnc = zapcore.NewJSONEncoder(encCfg)
		sampling = &zap.SamplingConfig{Initial: 100, Thereafter: 100}
	}

	enc, err := NewRedactingEncoder(innerEnc, cfg.Observability.Logging.RedactPatterns)
	if err != nil {
		return nil, fmt.Errorf("redacting encoder: %w", err)
	}

	atomLevel := zap.NewAtomicLevelAt(level)
	core := zapcore.NewCore(enc, sink, atomLevel)
	if sampling != nil {
		core = zapcore.NewSamplerWithOptions(core, time.Second, sampling.Initial, sampling.Thereafter)
	}

	logger := zap.New(core,
		zap.AddCaller(),
		zap.AddStacktrace(zap.ErrorLevel),
		zap.Fields(
			zap.String("service", cfg.Service.Name),
			zap.String("env", cfg.Service.Env),
			zap.String("region", cfg.Service.Region),
		),
	)
	return logger, nil
}

func parseLevel(s string) (zapcore.Level, error) {
	switch s {
	case "debug":
		return zapcore.DebugLevel, nil
	case "info":
		return zapcore.InfoLevel, nil
	case "warn":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.InfoLevel, errors.New("unknown log level: " + s)
	}
}

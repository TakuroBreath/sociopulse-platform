package config

import (
	"errors"
	"fmt"
)

// ObservabilityConfig — three-pillar settings (logs, metrics, traces).
// See spec §15.
type ObservabilityConfig struct {
	OTel    OTelConfig    `mapstructure:"otel"`
	Metrics MetricsConfig `mapstructure:"metrics"`
	Logging LoggingConfig `mapstructure:"logging"`
}

// OTelConfig points at the OTLP/gRPC collector and configures sampling.
type OTelConfig struct {
	Endpoint      string  `mapstructure:"endpoint"`
	SamplingRatio float64 `mapstructure:"sampling_ratio"`
	Insecure      bool    `mapstructure:"insecure"`
	ServiceName   string  `mapstructure:"service_name"`
}

// MetricsConfig governs the Prometheus exposition endpoint.
type MetricsConfig struct {
	Bind      string `mapstructure:"bind"`
	Namespace string `mapstructure:"namespace"`
}

// LoggingConfig holds log redaction patterns and structured-log sample rates.
type LoggingConfig struct {
	RedactPatterns  []string `mapstructure:"redact_patterns"`
	SampleInfoLogs  float64  `mapstructure:"sample_info_logs"`
	SampleDebugLogs float64  `mapstructure:"sample_debug_logs"`
}

func (o *ObservabilityConfig) validate() error {
	if o.Metrics.Bind == "" {
		return errors.New("metrics.bind required")
	}
	if o.Metrics.Namespace == "" {
		return errors.New("metrics.namespace required")
	}
	if o.OTel.SamplingRatio < 0 || o.OTel.SamplingRatio > 1 {
		return fmt.Errorf("otel.sampling_ratio must be in [0,1], got %v", o.OTel.SamplingRatio)
	}
	if o.Logging.SampleInfoLogs < 0 || o.Logging.SampleInfoLogs > 1 {
		return errors.New("logging.sample_info_logs must be in [0,1]")
	}
	if o.Logging.SampleDebugLogs < 0 || o.Logging.SampleDebugLogs > 1 {
		return errors.New("logging.sample_debug_logs must be in [0,1]")
	}
	return nil
}

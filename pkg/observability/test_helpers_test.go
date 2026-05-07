package observability

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sociopulse/platform/pkg/config"
)

// prometheusGauge is a tiny helper for metrics_test.
func prometheusGauge(t *testing.T, name, help string) prometheus.Gauge {
	t.Helper()
	return prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
}

// newTracerProviderWithRecorder builds a *sdktrace.TracerProvider with the
// supplied tracetest.SpanRecorder attached as a span processor so tests can
// verify span emission without a real OTLP listener.
func newTracerProviderWithRecorder(t *testing.T, serviceName, env string, rec *tracetest.SpanRecorder, ratio float64) *sdktrace.TracerProvider {
	t.Helper()
	res, err := newResource(context.Background(), serviceName, env)
	require.NoError(t, err)
	return sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(rec),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(res),
	)
}

// tracerCfgForTest builds a minimal Config with the OTel block populated.
func tracerCfgForTest(endpoint, service, env string, ratio float64) config.Config {
	cfg := config.DefaultDev()
	cfg.Service.Env = env
	cfg.Observability.OTel.Endpoint = endpoint
	cfg.Observability.OTel.ServiceName = service
	cfg.Observability.OTel.SamplingRatio = ratio
	cfg.Observability.OTel.Insecure = true
	return cfg
}

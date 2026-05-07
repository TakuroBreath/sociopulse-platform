package observability

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// NewTracer constructs an OTel TracerProvider configured with:
//
//   - an OTLP gRPC exporter pointing at endpoint,
//   - a head-based sampler whose ratio comes from
//     ObservabilityConfig.Tracing.SamplingRatio,
//   - a span processor that attaches service.environment and
//     service.version to every span,
//   - W3C TraceContext + Baggage propagators.
//
// The returned shutdown function flushes pending spans and closes the
// exporter; cmd/<binary>/main.go calls it from a defer.
//
// Concrete SDK wiring lands in Plan 02 Task 2.
func NewTracer(ctx context.Context, serviceName, endpoint string) (provider trace.TracerProvider, shutdown func(context.Context) error, err error) {
	panic("not implemented: see Plan 02 Task 2")
}

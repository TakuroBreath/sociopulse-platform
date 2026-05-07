package observability

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// NewMeter constructs an OTel MeterProvider feeding both Prometheus
// (scraped at /metrics on Config.Observability.Metrics.Bind) and the
// OTel collector. The returned shutdown function flushes pending
// metric exports and closes the exporters.
//
// Concrete SDK wiring lands in Plan 02 Task 2.
func NewMeter(ctx context.Context, serviceName, endpoint string) (provider metric.MeterProvider, shutdown func(context.Context) error, err error) {
	panic("not implemented: see Plan 02 Task 2")
}

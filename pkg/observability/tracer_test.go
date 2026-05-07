package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestNoopTracerReturnsNonNil(t *testing.T) {
	t.Parallel()
	tr := NoopTracer()
	assert.NotNil(t, tr)
}

// TestNewTracerProviderWithRecorder builds a tracer provider whose exporter is
// a synchronous in-memory recorder, so we can assert that spans are created
// without needing a live OTLP collector.
func TestNewTracerProviderWithRecorder(t *testing.T) {
	t.Parallel()
	rec := tracetest.NewSpanRecorder()
	tp := newTracerProviderWithRecorder(t, "test-service", "production", rec, 1.0)
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "unit-test-span")
	span.End()

	// Force flush to make sure the recorder sees the span.
	require.NoError(t, tp.ForceFlush(context.Background()))

	spans := rec.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "unit-test-span", spans[0].Name())
}

// TestNewTracerRejectsEmptyEndpoint verifies validation runs before any
// network calls.
func TestNewTracerRejectsEmptyEndpoint(t *testing.T) {
	t.Parallel()
	cfg := tracerCfgForTest("", "test", "production", 1.0)
	_, _, err := NewTracer(context.Background(), cfg)
	require.Error(t, err)
}

// TestNewTracerRejectsBadSamplingRatio verifies sampling-ratio validation.
func TestNewTracerRejectsBadSamplingRatio(t *testing.T) {
	t.Parallel()
	cfg := tracerCfgForTest("localhost:4317", "test", "production", 5.0)
	_, _, err := NewTracer(context.Background(), cfg)
	require.Error(t, err)
}

// TestNewTracerLazilyConstructsClient documents the contract: with the
// non-blocking grpc.NewClient API, construction succeeds even when the
// endpoint is unreachable; failures surface on first export.
func TestNewTracerLazilyConstructsClient(t *testing.T) {
	t.Parallel()
	cfg := tracerCfgForTest("localhost:0", "test", "production", 1.0)
	tracer, shutdown, err := NewTracer(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, tracer)
	require.NotNil(t, shutdown)
	t.Cleanup(func() {
		_ = shutdown(context.Background())
	})
}

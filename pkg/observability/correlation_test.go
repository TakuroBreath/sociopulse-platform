package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRequestIDRoundtripsThroughContext(t *testing.T) {
	t.Parallel()
	ctx := WithRequestID(context.Background(), "req-abc")
	got := RequestIDFromContext(ctx)
	assert.Equal(t, "req-abc", got)
}

func TestRequestIDFromContextReturnsEmptyWhenAbsent(t *testing.T) {
	t.Parallel()
	got := RequestIDFromContext(context.Background())
	assert.Empty(t, got)
}

func TestRequestIDFromContextHandlesWrongType(t *testing.T) {
	t.Parallel()
	// Different package's ctxKey would never collide with ours, but a plain
	// string key with the same name is what an attacker / accidental import
	// might do. Safety: we should only honour our private ctxKey.
	type otherKey string
	ctx := context.WithValue(context.Background(), otherKey("request_id"), "wrong")
	got := RequestIDFromContext(ctx)
	assert.Empty(t, got)
}

func TestTraceIDFromContextEmptyWhenNoSpan(t *testing.T) {
	t.Parallel()
	got := TraceIDFromContext(context.Background())
	assert.Empty(t, got)
}

func TestTraceIDFromContextReturnsHexWhenSpanIsActive(t *testing.T) {
	t.Parallel()
	rec := tracetest.NewSpanRecorder()
	tp := newTracerProviderWithRecorder(t, "trace-id-test", "production", rec, 1.0)
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	ctx, span := tp.Tracer("test").Start(context.Background(), "with-id")
	defer span.End()

	got := TraceIDFromContext(ctx)
	assert.Len(t, got, 32) // 16 bytes hex-encoded
	assert.Equal(t, span.SpanContext().TraceID().String(), got)
}

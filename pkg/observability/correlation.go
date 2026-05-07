package observability

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// ctxKey is unexported so callers cannot collide with us by accident.
type ctxKey int

const (
	requestIDKey ctxKey = iota + 1
)

// WithRequestID returns ctx annotated with the supplied request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the X-Request-ID value or "" if none was set.
// Only values inserted via WithRequestID round-trip; values stored under any
// other key type return "".
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// TraceIDFromContext returns the OTel trace id (32-char hex) or "" if no
// active span context lives on ctx.
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanIDFromContext returns the active span id (16-char hex) or "".
func SpanIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.SpanID().String()
}

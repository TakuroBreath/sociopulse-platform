package observability

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// requestIDHeader is the canonical wire name for the request id.
const requestIDHeader = "X-Request-Id"

// maxRequestIDLen caps the length of an inbound request id we accept. Longer
// values are replaced with a freshly-generated id to defend against header
// pollution.
const maxRequestIDLen = 128

// Middleware ordering (documented contract):
//
//	r.Use(
//	    RequestIDMiddleware(),       // 1. assign id, store on ctx + response header
//	    LoggingMiddleware(logger),   // 2. log entry/exit with request_id+trace_id
//	    TracingMiddleware(tracer),   // 3. start server-side span, inherit trace context
//	    MetricsMiddleware(metrics),  // 4. record duration, in-flight, counter
//	)
//
// RequestID must come first so logging/tracing/metrics can stamp every
// emission with the same id. Logging is set up before tracing so even spans
// that fail to start get a log line. Metrics is innermost because we want it
// closest to the handler and want to observe the post-handler status code.

// RequestIDMiddleware reads X-Request-Id from the incoming request, generates
// a UUIDv7 when absent or oversized, stores it on *gin.Context and the
// request context, and echoes it back on the response.
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if id == "" || len(id) > maxRequestIDLen {
			id = newRequestID()
		}
		c.Writer.Header().Set(requestIDHeader, id)
		c.Set("request_id", id)
		ctx := WithRequestID(c.Request.Context(), id)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// newRequestID returns a UUIDv7 string. Falls back to UUIDv4 if the v7
// generator fails (e.g. clock issues), so the request still gets an id.
func newRequestID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// LoggingMiddleware emits one structured log line per request with the field
// set documented in docs/architecture/06-observability.md (service,
// request_id, trace_id, span_id, http.method, http.route, http.status_code,
// duration_ms, error).
func LoggingMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		duration := time.Since(start)

		ctx := c.Request.Context()
		fields := []zap.Field{
			zap.String("request_id", RequestIDFromContext(ctx)),
			zap.String("trace_id", TraceIDFromContext(ctx)),
			zap.String("span_id", SpanIDFromContext(ctx)),
			zap.String("http.method", c.Request.Method),
			zap.String("http.route", routeOf(c)),
			zap.Int("http.status_code", c.Writer.Status()),
			zap.Int64("duration_ms", duration.Milliseconds()),
			zap.String("client_ip", c.ClientIP()),
		}
		if errs := c.Errors.ByType(gin.ErrorTypePrivate); len(errs) > 0 {
			fields = append(fields, zap.String("error", errs.String()))
			logger.Error("request failed", fields...)
			return
		}
		if c.Writer.Status() >= 500 {
			logger.Error("request 5xx", fields...)
			return
		}
		logger.Info("request", fields...)
	}
}

// TracingMiddleware extracts the W3C TraceContext header (the global
// propagator does that for us via the supplied tracer's provider), starts a
// server-side span named "<HTTP method> <route>", and records the standard
// HTTP attributes.
func TracingMiddleware(tracer trace.Tracer) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		spanName := c.Request.Method + " " + routeOf(c)
		ctx, span := tracer.Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", c.Request.Method),
				attribute.String("http.route", routeOf(c)),
				attribute.String("http.target", c.Request.URL.Path),
				attribute.String("http.scheme", c.Request.URL.Scheme),
			),
		)
		defer span.End()

		c.Request = c.Request.WithContext(ctx)
		c.Next()

		span.SetAttributes(attribute.Int("http.status_code", c.Writer.Status()))
		if c.Writer.Status() >= 500 {
			span.SetStatus(codes.Error, "5xx")
		}
	}
}

// MetricsMiddleware records sociopulse_http_request_duration_seconds,
// sociopulse_http_inflight_requests, and sociopulse_http_requests_total for
// every request.
func MetricsMiddleware(m *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		m.HTTPInflight.Inc()
		start := time.Now()
		c.Next()
		m.HTTPInflight.Dec()

		status := strconv.Itoa(c.Writer.Status())
		route := routeOf(c)
		m.HTTPRequestDuration.WithLabelValues(c.Request.Method, route, status).
			Observe(time.Since(start).Seconds())
		m.HTTPRequestsTotal.WithLabelValues(c.Request.Method, route, status).Inc()
	}
}

// routeOf returns the gin-resolved route template (e.g. "/users/:id"), or
// the raw URL path when no route matched.
func routeOf(c *gin.Context) string {
	if r := c.FullPath(); r != "" {
		return r
	}
	return c.Request.URL.Path
}

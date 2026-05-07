package observability

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// RequestIDMiddleware reads X-Request-Id from the incoming request,
// generates a UUIDv7 when absent, stores it on *gin.Context, and
// echoes it back in the response header. Downstream code reads the id
// via httputil.RequestIDFromContext.
func RequestIDMiddleware() gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 2")
}

// LoggingMiddleware emits one structured log line per request with
// the field set documented in docs/architecture/06-observability.md
// (service, request_id, trace_id, span_id, http.method, http.route,
// http.status_code, duration_ms, error).
func LoggingMiddleware(logger *zap.Logger) gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 2")
}

// TracingMiddleware extracts the W3C TraceContext header, starts a
// server-side span named "<module>.<HTTP method>.<route>", and records
// the standard HTTP attributes.
func TracingMiddleware() gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 2")
}

// MetricsMiddleware records sociopulse_http_request_duration_seconds
// and sociopulse_http_inflight_requests for every request.
func MetricsMiddleware() gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 2")
}

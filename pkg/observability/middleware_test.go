package observability

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sociopulse/platform/pkg/config"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ----- RequestID middleware -----

func TestRequestIDMiddlewareGeneratesIDWhenAbsent(t *testing.T) {
	t.Parallel()
	r := gin.New()
	r.Use(RequestIDMiddleware())
	r.GET("/x", func(c *gin.Context) {
		id := RequestIDFromContext(c.Request.Context())
		assert.NotEmpty(t, id)
		c.String(http.StatusOK, id)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, w.Header().Get("X-Request-Id"))
	assert.Equal(t, w.Header().Get("X-Request-Id"), w.Body.String())
}

func TestRequestIDMiddlewareEchoesIncomingHeader(t *testing.T) {
	t.Parallel()
	r := gin.New()
	r.Use(RequestIDMiddleware())
	r.GET("/y", func(c *gin.Context) {
		c.String(http.StatusOK, RequestIDFromContext(c.Request.Context()))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/y", nil)
	req.Header.Set("X-Request-Id", "client-supplied-id")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "client-supplied-id", w.Header().Get("X-Request-Id"))
	assert.Equal(t, "client-supplied-id", w.Body.String())
}

func TestRequestIDMiddlewareIgnoresOversizedHeader(t *testing.T) {
	t.Parallel()
	r := gin.New()
	r.Use(RequestIDMiddleware())
	r.GET("/z", func(c *gin.Context) {
		c.String(http.StatusOK, RequestIDFromContext(c.Request.Context()))
	})

	huge := strings.Repeat("a", 1024)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/z", nil)
	req.Header.Set("X-Request-Id", huge)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotEqual(t, huge, w.Body.String(), "oversized id must be replaced")
	assert.NotEmpty(t, w.Body.String())
}

// ----- Logging middleware -----

func TestLoggingMiddlewareEmitsStructuredLine(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(buf),
		zapcore.DebugLevel,
	))

	r := gin.New()
	r.Use(RequestIDMiddleware(), LoggingMiddleware(logger))
	r.GET("/hello", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	line := buf.String()
	assert.Contains(t, line, `"http.method":"GET"`)
	assert.Contains(t, line, `"http.route":"/hello"`)
	assert.Contains(t, line, `"http.status_code":200`)
	assert.Contains(t, line, `"request_id"`)
	assert.Contains(t, line, `"duration_ms"`)
}

// ----- Tracing middleware -----

func TestTracingMiddlewareCreatesSpan(t *testing.T) {
	t.Parallel()
	rec := tracetest.NewSpanRecorder()
	tp := newTracerProviderWithRecorder(t, "trace-mw-test", "production", rec, 1.0)
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	r := gin.New()
	r.Use(RequestIDMiddleware(), TracingMiddleware(tp.Tracer("test")))
	r.GET("/spanned", func(c *gin.Context) {
		// Span must be on the request context.
		traceID := TraceIDFromContext(c.Request.Context())
		assert.NotEmpty(t, traceID)
		c.String(http.StatusOK, traceID)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/spanned", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	spans := rec.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Name(), "/spanned")
	// Body contains the trace id (32 hex chars).
	assert.Len(t, w.Body.String(), 32)
}

// ----- Metrics middleware -----

func TestMetricsMiddlewareIncrementsCounters(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	m := NewMetrics(cfg)

	r := gin.New()
	r.Use(MetricsMiddleware(m))
	r.GET("/counted", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	for range 3 {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/counted", nil)
		r.ServeHTTP(w, req)
	}

	rec := httptest.NewRecorder()
	mreq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, mreq)
	body := rec.Body.String()
	assert.Contains(t, body, `sociopulse_http_requests_total{method="GET",path="/counted",status="200"} 3`)
	assert.Contains(t, body, `sociopulse_http_request_duration_seconds_bucket`)
}

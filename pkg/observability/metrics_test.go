package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/config"
)

func TestMetricsHandlerExposesNamespacedSeries(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	m := NewMetrics(cfg)
	m.HTTPRequestDuration.WithLabelValues("GET", "/healthz", "200").Observe(0.01)
	m.HTTPInflight.Set(0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "sociopulse_http_request_duration_seconds_bucket")
	assert.Contains(t, body, "sociopulse_http_inflight_requests")
}

func TestMetricsRegisterAddsCustomCollector(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	m := NewMetrics(cfg)
	g := prometheusGauge(t, "sociopulse_test_custom", "test")
	require.NoError(t, m.Register(g))
}

func TestMetricsRegistryIsolatedFromGlobalDefault(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	m := NewMetrics(cfg)
	// Registering the same gauge twice must error (registry is its own).
	g := prometheusGauge(t, "sociopulse_test_isolated", "test")
	require.NoError(t, m.Register(g))
	require.Error(t, m.Register(g))

	// And another fresh registry should accept the same name without conflict
	// because it's a different registry entirely.
	m2 := NewMetrics(cfg)
	g2 := prometheusGauge(t, "sociopulse_test_isolated", "test")
	require.NoError(t, m2.Register(g2))
}

func TestMetricsExposesGoAndProcessCollectors(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	m := NewMetrics(cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	// The Go runtime collector exposes go_goroutines; the process collector
	// exposes process_start_time_seconds (when supported by the OS).
	assert.Contains(t, body, "go_goroutines")
}

func TestMetricsNamespaceRespected(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultDev()
	cfg.Observability.Metrics.Namespace = "custom_ns"
	m := NewMetrics(cfg)
	m.HTTPInflight.Inc()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	assert.Contains(t, body, "custom_ns_http_inflight_requests")
	assert.NotContains(t, body, "sociopulse_http_inflight_requests")
}

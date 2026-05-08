// Package main — composition root for the cmd/api monolith.
//
// server.go extracts HTTP-engine and metrics-listener wiring from main.go so
// run() reads as a flat orchestration of named build steps rather than a wall
// of inline gin/middleware setup.
package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/healthz"
	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/observability"
)

// readinessTimeout is the deadline applied to the parallel /readyz probe.
// Operators expect readiness to fail fast when a dependency is wedged, so we
// keep this short and well under the kubelet's default probe timeout.
const readinessTimeout = 2 * time.Second

// buildHTTPServer assembles the gin engine, registers liveness/readiness
// endpoints, and returns an *http.Server ready to ListenAndServe.
//
// The /metrics endpoint is intentionally NOT mounted on this engine — it lives
// on a separate listener (built by buildMetricsServer) so internal scraping
// traffic does not contend with public request middleware (auth, rate limits,
// tracing).
//
// healthz checks are passed in by the caller. During Plan 02, real DB/Redis/
// NATS clients are not yet constructed (those come in Plans 03 and 04), so
// the caller passes an empty list and /readyz returns 200 immediately.
func buildHTTPServer(
	cfg config.Config,
	logger *zap.Logger,
	tracer trace.Tracer,
	metrics *observability.Metrics,
	checks []healthz.Checker,
) (*http.Server, *gin.Engine) {
	if cfg.Service.Env == "development" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()

	// SECURITY: gin's default trusts every proxy (0.0.0.0/0), which lets
	// any client spoof their X-Forwarded-For and bypass per-IP rate
	// limiting / lockout. Pin the trusted-proxy list to whatever the
	// load balancer's CIDR is. An empty config list locks gin down to
	// "trust no proxy" — strictest possible default. Errors here are
	// fatal: a misconfigured TrustedProxies entry is a security
	// regression we must not silently tolerate.
	if err := r.SetTrustedProxies(cfg.HTTP.TrustedProxies); err != nil {
		logger.Fatal("HTTP: SetTrustedProxies",
			zap.Strings("trusted_proxies", cfg.HTTP.TrustedProxies),
			zap.Error(err),
		)
	}

	r.Use(
		gin.Recovery(),
		observability.RequestIDMiddleware(),
		observability.LoggingMiddleware(logger),
		observability.TracingMiddleware(tracer),
		observability.MetricsMiddleware(metrics),
	)

	// Health endpoints — raw http.Handlers from internal/healthz, wrapped via
	// gin.WrapH so they sit on the same gin engine and inherit middleware
	// (request id + logging). The middleware is harmless for these probes and
	// the consistency keeps operator dashboards uniform.
	r.GET("/healthz", gin.WrapH(healthz.NewLivenessHandler()))
	r.GET("/readyz", gin.WrapH(healthz.NewReadinessHandler(readinessTimeout, checks...)))

	srv := &http.Server{
		Addr:              cfg.HTTP.Bind,
		Handler:           r,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}
	return srv, r
}

// buildMetricsServer returns an *http.Server that exposes /metrics on the
// configured bind address. Keeping Prometheus on its own listener:
//   - prevents scrape traffic from showing up in HTTP request metrics
//     (avoids the recursive metric self-pollution problem),
//   - lets us bind /metrics to an internal interface in production while
//     the public HTTP listener stays on the public one,
//   - simplifies authn — the metrics port can sit behind a NetworkPolicy
//     instead of inheriting public auth middleware.
func buildMetricsServer(cfg config.Config, metrics *observability.Metrics) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(
		metrics.Registry,
		promhttp.HandlerOpts{Timeout: 5 * time.Second, EnableOpenMetrics: true},
	))
	return &http.Server{
		Addr:              cfg.Observability.Metrics.Bind,
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

// listenAndServe wraps srv.ListenAndServe to translate the expected
// http.ErrServerClosed into a nil error. Goroutine bodies that block on this
// can be added directly to errgroup without conditional branches.
func listenAndServe(srv *http.Server) error {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// shutdownServer drives http.Server.Shutdown with the supplied deadline,
// logging the outcome but never aborting the broader shutdown sequence.
func shutdownServer(ctx context.Context, srv *http.Server, name string, logger *zap.Logger) {
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("http server shutdown",
			zap.String("server", name),
			zap.Error(err),
		)
		return
	}
	logger.Info("http server shut down", zap.String("server", name))
}

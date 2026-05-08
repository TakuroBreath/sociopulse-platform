// Package main — composition root helpers for cmd/telephony-bridge.
//
// server.go isolates HTTP wiring from main.go so run() reads as a flat
// orchestration of named build steps rather than a wall of inline gin
// configuration.
package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/healthz"
	"github.com/sociopulse/platform/internal/telephony/pool"
	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/observability"
)

// buildHealthServer assembles the gin engine that serves /healthz and
// /readyz. Mirrors cmd/api's shape (RequestID + Logging + Tracing +
// Metrics middleware) so dashboards and log indices look the same.
//
// The metrics endpoint is intentionally NOT mounted on this engine — it
// lives on a separate listener (built by buildMetricsServer) so internal
// scraping traffic does not contend with public request middleware.
func buildHealthServer(
	bind string,
	cfg config.Config,
	logger *zap.Logger,
	tracer trace.Tracer,
	metrics *observability.Metrics,
	checks []healthz.Checker,
) *http.Server {
	if cfg.Service.Env == "development" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()

	// SECURITY: lock the trusted-proxy list per cmd/api convention. The
	// bridge sits behind the same load balancer; an empty list locks gin
	// down to "trust no proxy" (strictest secure default).
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

	r.GET("/healthz", gin.WrapH(healthz.NewLivenessHandler()))
	r.GET("/readyz", gin.WrapH(healthz.NewReadinessHandler(readinessTimeout, checks...)))

	return &http.Server{
		Addr:              bind,
		Handler:           r,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}
}

// buildMetricsServer returns the *http.Server that exposes /metrics on the
// configured bind address. Same isolation rationale as cmd/api: scrape
// traffic stays off the public listener and out of HTTP request metrics.
//
// The /metrics endpoint is mounted on a gin engine (per project directive
// "use gin and its ecosystem") via gin.WrapH(promhttp.HandlerFor(...)).
// Operator dashboards inherit the same RequestID + tracing middleware.
func buildMetricsServer(cfg config.Config, metrics *observability.Metrics) *http.Server {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	if err := r.SetTrustedProxies(nil); err != nil {
		// SetTrustedProxies(nil) is the strictest path and never errors in
		// gin v1.x; keep the check for forward compatibility.
		panic("metrics SetTrustedProxies(nil): " + err.Error())
	}
	r.GET("/metrics", gin.WrapH(promhttp.HandlerFor(
		metrics.Registry,
		promhttp.HandlerOpts{Timeout: 5 * time.Second, EnableOpenMetrics: true},
	)))
	return &http.Server{
		Addr:              cfg.Observability.Metrics.Bind,
		Handler:           r,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

// listenAndServe wraps srv.ListenAndServe to translate the expected
// http.ErrServerClosed into a nil error. Same helper as cmd/api/server.go.
func listenAndServe(srv *http.Server) error {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// shutdownServer drives http.Server.Shutdown with the supplied deadline,
// logging the outcome but never aborting the broader shutdown sequence.
// Same helper as cmd/api/server.go.
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

// redisPinger adapts *redis.Client to healthz/checks.RedisPinger by ignoring
// the StatusCmd return of Ping. Inlined here because the bridge is the only
// caller; promoting it to pkg/ would be premature.
type redisPinger struct {
	c *redis.Client
}

// Ping implements healthchecks.RedisPinger.
func (r redisPinger) Ping(ctx context.Context) error {
	return r.c.Ping(ctx).Err()
}

// eslPoolCheck adapts *pool.ESLPool to healthz.Checker. /readyz returns
// 503 when no FS node is reachable, matching the kubelet contract that
// drains pods with no useful work to do.
type eslPoolCheck struct {
	pool *pool.ESLPool
}

// Name reports the dependency identifier surfaced in /readyz output.
func (eslPoolCheck) Name() string { return "esl_pool" }

// Check returns nil iff at least one configured FS node is healthy.
func (e eslPoolCheck) Check(_ context.Context) error {
	if e.pool == nil {
		return errors.New("esl_pool: not initialised")
	}
	if !e.pool.AnyHealthy() {
		return errors.New("esl_pool: no healthy FreeSWITCH node")
	}
	return nil
}

// natsConnAdapter narrows *nats.Conn to the healthchecks.NATSConn surface.
// healthz/checks declares Status() int — its tests stub a fake — but the
// real *nats.Conn returns the strongly-typed nats.Status enum. The adapter
// shifts the enum into its underlying int representation so the check can
// embed the numeric status in error output.
type natsConnAdapter struct {
	c *nats.Conn
}

// IsConnected satisfies healthchecks.NATSConn.
func (a natsConnAdapter) IsConnected() bool { return a.c.IsConnected() }

// Status satisfies healthchecks.NATSConn by widening nats.Status (an int
// alias) into a plain int. The underlying value matches the numeric enum
// documented on healthchecks.NATSCheck.
func (a natsConnAdapter) Status() int { return int(a.c.Status()) }

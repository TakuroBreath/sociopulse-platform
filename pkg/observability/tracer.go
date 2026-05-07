package observability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sociopulse/platform/pkg/config"
)

// TracerShutdown is the function returned by NewTracer. Call at process exit.
type TracerShutdown func(context.Context) error

// NewTracer initialises the global OTel TracerProvider with an OTLP/gRPC
// exporter pointing at cfg.Observability.OTel.Endpoint. Sampling is parent-based
// with a TraceIDRatio root sampler at SamplingRatio.
//
// The returned Tracer is the canonical "github.com/sociopulse/platform" tracer.
// The shutdown function flushes pending spans and closes the exporter; cmd/api
// calls it from a defer.
func NewTracer(ctx context.Context, cfg config.Config) (trace.Tracer, TracerShutdown, error) {
	if cfg.Observability.OTel.Endpoint == "" {
		return nil, nil, errors.New("observability.otel.endpoint is required")
	}
	if cfg.Observability.OTel.SamplingRatio < 0 || cfg.Observability.OTel.SamplingRatio > 1 {
		return nil, nil, fmt.Errorf("observability.otel.sampling_ratio must be in [0,1], got %v",
			cfg.Observability.OTel.SamplingRatio)
	}

	// grpc.NewClient is the modern, non-blocking constructor. It does NOT
	// dial until first RPC; this lets the OTel batch processor handle
	// downstream availability with retries instead of blocking startup.
	var transportCreds credentials.TransportCredentials
	if cfg.Observability.OTel.Insecure {
		transportCreds = insecure.NewCredentials()
	} else {
		transportCreds = credentials.NewTLS(nil)
	}
	conn, err := grpc.NewClient(cfg.Observability.OTel.Endpoint,
		grpc.WithTransportCredentials(transportCreds),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create otlp grpc client: %w", err)
	}

	exp, err := otlptrace.New(ctx, otlptracegrpc.NewClient(
		otlptracegrpc.WithGRPCConn(conn),
	))
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	res, err := newResource(ctx, cfg.Observability.OTel.ServiceName, cfg.Service.Env)
	if err != nil {
		_ = exp.Shutdown(ctx)
		_ = conn.Close()
		return nil, nil, fmt.Errorf("otel resource: %w", err)
	}

	tp := newTracerProvider(exp, res, cfg.Observability.OTel.SamplingRatio)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	tracer := tp.Tracer("github.com/sociopulse/platform")
	shutdown := func(ctx context.Context) error {
		// Best-effort: flush spans then close the gRPC channel.
		shutdownErr := tp.Shutdown(ctx)
		closeErr := conn.Close()
		return errors.Join(shutdownErr, closeErr)
	}
	return tracer, shutdown, nil
}

// newResource builds the OTel Resource attached to every span.
func newResource(ctx context.Context, serviceName, env string) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion("dev"),
			semconv.DeploymentEnvironment(env),
		),
	)
}

// newTracerProvider wires the SDK pieces together. Extracted so tests can
// substitute an in-memory exporter via newTracerProviderWithExporter.
func newTracerProvider(exp sdktrace.SpanExporter, res *resource.Resource, ratio float64) *sdktrace.TracerProvider {
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(2*time.Second),
			sdktrace.WithMaxExportBatchSize(512),
		),
		sdktrace.WithSampler(sampler),
		sdktrace.WithResource(res),
	)
}

// NoopTracer is a tracer that records nothing. Useful for tests where the
// caller does not want to spin up a real OTLP listener.
func NoopTracer() trace.Tracer {
	return otel.Tracer("noop")
}

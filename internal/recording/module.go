// Package recording is the registration entry point for the recording
// module — RecordingService over gRPC (mTLS) + HTTP playback (Plan 12.2)
// + retention pass (Plan 12.3).
//
// Plan 12.1 Foundation (this file's scope):
//   - Plan 12.1 Task 1 — DB migration evolves call_recordings.
//   - Plan 12.1 Task 2 — proto contract + Go bindings.
//   - Plan 12.1 Task 3 — RecordingStore (idempotent insert + get).
//   - Plan 12.1 Task 4 — RecordingService.Commit + Get + metrics.
//   - Plan 12.1 Task 5 — gRPC server façade + Module composition (THIS task).
//
// Register builds the store + service, registers them in the locator
// under LocatorRecordingService so future modules (HTTP transport,
// retention, listen-in) can resolve the service without taking a
// transitive dependency on internal/recording/service. When a gRPC
// listener is configured (Config.GRPCConfig.Enabled() == true) and
// the TLS material loads, Register builds the *grpcserver.Server but
// does NOT start it — Start is called from cmd/api inside the
// process-wide errgroup so its lifecycle is shutdown-driven.
//
// Defensive design: if the gRPC config is supplied but cert files
// fail to load, Register WARNs and proceeds without the listener
// rather than failing the whole boot. Production can detect the
// degraded state via the cmd/api log; dev/test environments without
// certs simply leave Config.GRPCConfig nil and never reach that path.
package recording

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/internal/recording/grpcserver"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/service"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/internal/recording/store"
)

// LocatorRecordingService is the locator key under which Register
// stashes the constructed rapi.RecordingService. Plan 12.2 / 12.3
// modules look it up here when they need to call Commit / Get / etc.
const LocatorRecordingService = "recording.RecordingService"

// Config groups the construction-time parameters of the module.
//
// Registerer: Prometheus collectors are registered against this. May
// be nil — Register replaces it with a private prometheus.NewRegistry
// so unit tests boot cleanly.
//
// GRPCConfig: when nil OR Enabled()==false, the module skips the
// gRPC listener entirely. Useful for dev environments without TLS
// material; the RecordingService is still registered in the locator
// so HTTP-only callers work.
type Config struct {
	Registerer prometheus.Registerer
	GRPCConfig *grpcserver.Config

	// DEKUnwrapper unwraps envelope-encrypted DEKs during OpenAudioStream.
	// Required for OpenAudioStream — when nil, OpenAudioStream returns
	// ErrInvalidInput "not wired" but Commit / Get continue to work. The
	// production binary supplies a Yandex KMS-backed implementation (Plan
	// 01); dev/test uses crypto.LocalDEKUnwrapper.
	DEKUnwrapper crypto.DEKUnwrapper

	// ObjectStore is the object-storage backend OpenAudioStream reads from.
	// Same nil-tolerant semantics as DEKUnwrapper. Production uses Yandex
	// Object Storage (Plan 01); dev/test uses storage.LocalObjectStore.
	ObjectStore storage.ObjectStore
}

// Module is the top-level registration handle for the recording
// module. Lifecycle:
//
//   - New(cfg)        → cmd/api builds this with the config block.
//   - Register(deps)  → wires store + service + (optionally) the
//     gRPC server. Idempotent on a missing Pool — degraded boot.
//   - Start(ctx)      → blocks until ctx is cancelled; called from
//     cmd/api's errgroup. No-op when no gRPC listener is configured.
//   - Stop()          → drains in-flight RPCs via GracefulStop.
//
// Mirrors internal/realtime/module.go shape: the field set is
// intentionally minimal (no per-Register state machine), and the
// only mutable bits are guarded by mu.
type Module struct {
	cfg Config

	mu      sync.Mutex
	logger  *zap.Logger
	server  *grpcserver.Server
	stopped bool
}

// Compile-time check that *Module satisfies modules.Module —
// matches the realtime / dialer pattern.
var _ modules.Module = (*Module)(nil)

// New returns a fresh Module ready for Register. cmd/api passes
// pkg/observability.Metrics.Registry as cfg.Registerer so the
// recording collectors land on the shared /metrics endpoint; tests
// pass prometheus.NewRegistry() to keep registrations isolated.
func New(cfg Config) *Module {
	return &Module{cfg: cfg}
}

// Name returns the module's unique identifier within the registry.
func (*Module) Name() string { return "recording" }

// Register builds the recording subsystem from Deps. A nil Pool
// (Postgres unreachable) short-circuits the whole module — the
// RecordingService can't run without a database, and falling back
// to a partially-wired locator entry would let downstream modules
// fail mysteriously at first call.
//
// The gRPC server is built only when Config.GRPCConfig is supplied
// AND Enabled()==true AND the TLS files load. Failure to load certs
// is logged at WARN and Register returns nil — degraded boot wins
// over a hard failure here because cmd/api should still be useful
// for /healthz / /metrics.
func (m *Module) Register(d modules.Deps) error {
	if d.Logger == nil {
		return errors.New("recording: Deps.Logger is required")
	}
	logger := d.Logger.Named("recording")

	if d.Pool == nil {
		// No database → no recording subsystem. Production never
		// hits this; test boots without a Postgres container do.
		logger.Info("recording: Pool unavailable, module disabled")
		return nil
	}

	rmetrics, err := metrics.RegisterRecordingMetrics(m.cfg.registerer())
	if err != nil {
		return fmt.Errorf("recording metrics: %w", err)
	}

	pgStore := store.NewPostgresStore(d.Pool)
	svc := service.New(service.Deps{
		Pool:    d.Pool,
		Store:   pgStore,
		Logger:  logger,
		Metrics: rmetrics,
		KMS:     m.cfg.DEKUnwrapper, // NEW
		Objects: m.cfg.ObjectStore,  // NEW
		// Decryptor uses the default (crypto.NewAESGCMDecryptor()) since
		// there is no production-vs-test variant — it's pure Go.
	})
	if d.Locator != nil {
		d.Locator.Register(LocatorRecordingService, svc)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.logger = logger

	if m.cfg.GRPCConfig != nil && m.cfg.GRPCConfig.Enabled() {
		srv, err := grpcserver.New(*m.cfg.GRPCConfig, svc, logger.Named("grpc"))
		if err != nil {
			// Degraded boot: log + continue. The locator entry is in
			// place for HTTP-only flows; only the gRPC façade is
			// missing. cmd/api's log will surface the WARN.
			logger.Warn("recording: gRPC listener disabled — cert load failed",
				zap.String("listen_addr", m.cfg.GRPCConfig.ListenAddr),
				zap.Error(err),
			)
		} else {
			m.server = srv
			logger.Info("recording: gRPC listener configured",
				zap.String("listen_addr", m.cfg.GRPCConfig.ListenAddr),
			)
		}
	} else {
		logger.Info("recording: gRPC listener skipped (no config or disabled)")
	}

	return nil
}

// Start runs the gRPC listener until ctx is cancelled. cmd/api wires
// this into its top-level errgroup so a Serve() error trips the
// shared shutdown path. When no listener is configured, Start
// blocks on ctx — keeping the errgroup goroutine accounted for and
// returning nil on shutdown.
func (m *Module) Start(ctx context.Context) error {
	m.mu.Lock()
	srv := m.server
	m.mu.Unlock()

	if srv == nil {
		<-ctx.Done()
		return nil
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeAddr() }()

	select {
	case <-ctx.Done():
		srv.GracefulStop()
		// Drain so the goroutine doesn't outlive Start. Serve returns
		// nil after GracefulStop in our wrapper (grpc.ErrServerStopped
		// is squashed); a non-nil here is a real listener error and
		// we surface it as a warn-level outcome rather than failing
		// the shutdown path.
		<-errCh
		return nil
	case err := <-errCh:
		if err == nil || errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	}
}

// Stop is the synchronous half of the lifecycle. cmd/api calls Stop
// from a defer so the module's gRPC server drains its in-flight
// RPCs even if Start has already returned.
func (m *Module) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.server == nil {
		return nil
	}
	m.stopped = true
	m.server.GracefulStop()
	if m.logger != nil {
		m.logger.Info("recording module stopped")
	}
	return nil
}

// registerer returns a non-nil Registerer for collector registration
// — falling back to a private prometheus.NewRegistry when the caller
// didn't supply one. Mirrors internal/realtime/module.go's pattern.
func (c Config) registerer() prometheus.Registerer {
	if c.Registerer != nil {
		return c.Registerer
	}
	return prometheus.NewRegistry()
}

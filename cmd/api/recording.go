package main

import (
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/recording/grpcserver"
	recwire "github.com/sociopulse/platform/internal/recording/wire"
	"github.com/sociopulse/platform/pkg/config"
)

// recordingGRPCConfig translates the YAML recording.* block into the
// grpcserver.Config the recording module expects. Returns nil when
// recording.enabled is false — the module then skips the listener and
// only registers RecordingService in the locator. Plan 12.1 Task 5.
//
// We translate to a *grpcserver.Config (rather than passing the YAML
// type through) so the recording module's import edge stays
// recording → grpcserver without pulling pkg/config in.
//
// The Local* DEKUnwrapper + ObjectStore wiring lives in
// internal/recording/wire (Plan 12.4 Task 5) so cmd/api and cmd/worker
// share one helper. cmd/api builds via wire.LocalPorts directly in
// run().
func recordingGRPCConfig(c config.RecordingConfig) *grpcserver.Config {
	if !c.Enabled {
		return nil
	}
	return &grpcserver.Config{
		ListenAddr:   c.GRPCListenAddr,
		TLSCertFile:  c.TLSCertFile,
		TLSKeyFile:   c.TLSKeyFile,
		TLSCAFile:    c.TLSCAFile,
		MaxRecvBytes: c.MaxRecvBytes,
		Timeout:      c.Timeout,
	}
}

// buildRecordingPorts is the indirection the smoke build tag uses to
// inject a pre-populated *recwire.Ports into cmd/api boot. Production
// builds (no smoke tag) compile smoke_overrides_prod.go which leaves
// the atomic empty — Load returns nil — so this function falls through
// to recwire.LocalPorts unchanged.
//
// Smoke builds (//go:build smoke) compile smoke_overrides.go which
// exposes SetSmokeRecordingPorts; smoke tests call it BEFORE bootAPI
// to install a shared instance. At boot, this function returns the
// override and the recording handler reads back the same in-memory blob
// the test pre-Put.
//
// The override is consulted via atomic.Pointer.Load so a future caller
// of SetSmokeRecordingPorts from a different goroutine doesn't trip
// `-race` (Plan 21b Task 1 review fix-up — the previous plain pointer
// raced even though canonical usage publishes before bootAPI starts).
//
// Plan 21b § 2.6 references this seam by name; the alternative would be
// to scatter `if smokeOverrideRecordingPorts != nil` checks at every
// recwire.LocalPorts call site.
func buildRecordingPorts(cfg config.RecordingConfig, logger *zap.Logger) (*recwire.Ports, error) {
	if p := smokeOverrideRecordingPorts.Load(); p != nil {
		return p, nil
	}
	return recwire.LocalPorts(cfg, logger)
}

package main

import (
	"github.com/sociopulse/platform/internal/recording/grpcserver"
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

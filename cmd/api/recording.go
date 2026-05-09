package main

import (
	"encoding/hex"
	"fmt"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/internal/recording/grpcserver"
	"github.com/sociopulse/platform/internal/recording/storage"
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
// recordingPorts builds the Local* DEKUnwrapper + ObjectStore for now.
// Plan 01 (Yandex infra) will add a -tags=yandex_kms / -tags=yandex_s3
// branch that returns the SDK-backed adapters instead.
//
// In v1 the KEK material is sourced from configuration: cfg.LocalKEKs is
// a map[kms_key_id]hexEncodedKEK. Production deployments either set
// this to a single platform-wide test KEK (dev) OR leave it empty and
// supply the real Yandex-tagged binary instead. An empty map is NOT a
// hard error — it just means OpenAudioStream will fail at the KMS step
// for every recording until the real adapter lands.
func recordingPorts(cfg config.RecordingConfig, logger *zap.Logger) (crypto.DEKUnwrapper, storage.ObjectStore, error) {
	keks := make(map[string][]byte, len(cfg.LocalKEKs))
	for keyID, hexKEK := range cfg.LocalKEKs {
		kek, err := hex.DecodeString(hexKEK)
		if err != nil {
			return nil, nil, fmt.Errorf("recording: decode local KEK %q: %w", keyID, err)
		}
		if len(kek) != 32 {
			return nil, nil, fmt.Errorf("recording: local KEK %q must be 32 bytes for AES-256 (got %d)", keyID, len(kek))
		}
		keks[keyID] = kek
	}
	if len(keks) == 0 {
		logger.Warn("recording: no local KEKs configured — OpenAudioStream will fail until Plan 01 wires Yandex KMS")
	}
	return crypto.NewLocalDEKUnwrapper(keks), storage.NewLocalObjectStore(), nil
}

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

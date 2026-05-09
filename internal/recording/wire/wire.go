// Package wire centralises construction of the recording module's
// production ports — the DEKUnwrapper used to unwrap envelope-encrypted
// DEKs and the ObjectStore that holds the audio blobs. Today the
// in-tree implementations are crypto.LocalDEKUnwrapper and
// storage.LocalObjectStore (dev/test); Plan 01 will swap these for
// Yandex KMS + Yandex Object Storage adapters via build tags.
//
// Both cmd/api (Plan 12.2 Task 5) and cmd/worker (Plan 12.4 Task 5)
// need the same wiring — cmd/api hosts RecordingService.OpenAudioStream
// and cmd/worker hosts the retention + integrity passes. Centralising
// the build keeps the hex-decode + length validation logic in one
// place and makes the future Yandex-tagged branch a single-file edit.
//
// The dependency arrow is cmd/api + cmd/worker → internal/recording/wire
// → internal/recording/{crypto, storage}. The wire package does NOT
// import internal/recording itself (Module + Register live one level
// above) so there is no circular-dep risk; wire imports the leaf ports
// and pkg/config only.
package wire

import (
	"encoding/hex"
	"fmt"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/pkg/config"
)

// kekByteLen is the AES-256 key length in bytes (32). Hex-encoded KEKs
// are required to decode to exactly this many bytes — anything else is
// a configuration bug we surface at boot rather than at first
// OpenAudioStream call.
const kekByteLen = 32

// Ports bundles the two construction-time dependencies the recording
// service needs to fulfil OpenAudioStream and the integrity worker's
// VerifyChecksum: a DEKUnwrapper that turns a wrapped DEK back into
// the AES key bytes, and an ObjectStore that streams the encrypted
// audio.
//
// The struct is intentionally tiny — both ports are interfaces, so
// production replacements (Yandex KMS / Object Storage) plug in
// without changing the type. Code that needs a default-constructed
// Ports can call LocalPorts; code that wants to inject mocks (tests)
// constructs the struct literally.
type Ports struct {
	// DEK is the DEKUnwrapper used by RecordingService.OpenAudioStream
	// to recover the per-recording DEK from its envelope. Required
	// (when Ports is non-nil); nil DEK on a non-nil Ports is a wiring
	// bug.
	DEK crypto.DEKUnwrapper

	// Objects is the audio blob backend. Required on a non-nil Ports.
	Objects storage.ObjectStore
}

// LocalPorts builds the dev/test variant of Ports from
// config.RecordingConfig.LocalKEKs (a map[kms_key_id]hex-encoded-32-byte-KEK)
// plus the in-tree LocalObjectStore.
//
// Empty / nil LocalKEKs map → returns nil Ports + nil error and emits a
// WARN log. Callers MUST treat a nil Ports as "the recording paths
// requiring crypto / object storage are unavailable" and degrade
// accordingly:
//
//   - cmd/api passes nil DEKUnwrapper / ObjectStore to
//     recording.Config; OpenAudioStream then returns
//     api.ErrInvalidInput "not wired" until Plan 01 lands the
//     Yandex-tagged binary.
//   - cmd/worker skips the retention + integrity worker registration
//     entirely (the retention pass needs ObjectStore for hard-delete;
//     the integrity pass needs the service which needs DEKUnwrapper +
//     ObjectStore).
//
// A non-empty LocalKEKs map with any malformed entry (bad hex, wrong
// length) returns an error — operators should fix the config rather
// than silently fall through to the "not wired" path.
func LocalPorts(cfg config.RecordingConfig, logger *zap.Logger) (*Ports, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if len(cfg.LocalKEKs) == 0 {
		logger.Warn("recording: no local KEKs configured — DEK unwrap + object store unavailable until Plan 01 wires Yandex KMS")
		return nil, nil
	}
	keks := make(map[string][]byte, len(cfg.LocalKEKs))
	for keyID, hexKEK := range cfg.LocalKEKs {
		kek, err := hex.DecodeString(hexKEK)
		if err != nil {
			return nil, fmt.Errorf("recording: decode local KEK %q: %w", keyID, err)
		}
		if len(kek) != kekByteLen {
			return nil, fmt.Errorf(
				"recording: local KEK %q must be %d bytes for AES-256 (got %d)",
				keyID, kekByteLen, len(kek),
			)
		}
		keks[keyID] = kek
	}
	return &Ports{
		DEK:     crypto.NewLocalDEKUnwrapper(keks),
		Objects: storage.NewLocalObjectStore(),
	}, nil
}

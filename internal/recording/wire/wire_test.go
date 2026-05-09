package wire_test

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/recording/wire"
	"github.com/sociopulse/platform/pkg/config"
)

// TestLocalPorts_EmptyMapReturnsNil — empty LocalKEKs is the dev/test
// default; the caller (cmd/api or cmd/worker) is expected to treat the
// nil Ports as a degraded-boot signal.
func TestLocalPorts_EmptyMapReturnsNil(t *testing.T) {
	t.Parallel()
	logger := zaptest.NewLogger(t)
	ports, err := wire.LocalPorts(config.RecordingConfig{}, logger)
	require.NoError(t, err)
	require.Nil(t, ports, "empty LocalKEKs should yield nil Ports + WARN")
}

// TestLocalPorts_NilLoggerSafe — wire.LocalPorts must accept a nil
// logger (degrades to zap.Nop) so callers in test fixtures don't have
// to thread one in.
func TestLocalPorts_NilLoggerSafe(t *testing.T) {
	t.Parallel()
	ports, err := wire.LocalPorts(config.RecordingConfig{}, nil)
	require.NoError(t, err)
	require.Nil(t, ports)
}

// TestLocalPorts_HexDecodeAndLengthValidation covers the boot-time
// fail-fast paths: bad hex / wrong length / odd-length string all
// surface as a clear error from LocalPorts.
func TestLocalPorts_HexDecodeAndLengthValidation(t *testing.T) {
	t.Parallel()
	logger := zaptest.NewLogger(t)

	cases := []struct {
		name   string
		hexKEK string
		ok     bool
	}{
		{"valid_32_bytes", hex.EncodeToString(make([]byte, 32)), true},
		{"too_short", hex.EncodeToString(make([]byte, 16)), false},
		{"too_long", hex.EncodeToString(make([]byte, 64)), false},
		// odd-length hex string — hex.DecodeString rejects non-even length.
		{"odd_hex", "0123456789abcdef0123456789abcde", false},
		{"non_hex", "z" + hex.EncodeToString(make([]byte, 32))[1:], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.RecordingConfig{LocalKEKs: map[string]string{"kek-1": tc.hexKEK}}
			_, err := wire.LocalPorts(cfg, logger)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// TestLocalPorts_PopulatedMapBuildsLocalUnwrapper — happy path: a
// well-formed KEK map yields a non-nil Ports with both fields wired.
func TestLocalPorts_PopulatedMapBuildsLocalUnwrapper(t *testing.T) {
	t.Parallel()
	logger := zaptest.NewLogger(t)
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	cfg := config.RecordingConfig{
		LocalKEKs: map[string]string{"kek-test": hex.EncodeToString(kek)},
	}
	ports, err := wire.LocalPorts(cfg, logger)
	require.NoError(t, err)
	require.NotNil(t, ports)
	require.NotNil(t, ports.DEK)
	require.NotNil(t, ports.Objects)
}

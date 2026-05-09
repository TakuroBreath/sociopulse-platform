package main

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/pkg/config"
)

func TestRecordingPorts_EmptyMapWarnsButReturnsBothPorts(t *testing.T) {
	t.Parallel()
	logger := zaptest.NewLogger(t)
	kms, objects, err := recordingPorts(config.RecordingConfig{}, logger)
	require.NoError(t, err)
	require.NotNil(t, kms, "DEKUnwrapper must be non-nil even with empty KEK map")
	require.NotNil(t, objects, "ObjectStore must be non-nil")
}

func TestRecordingPorts_HexDecodeAndLengthValidation(t *testing.T) {
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
		{"odd_hex", "0123456789abcdef0123456789abcde", false}, // 31 hex chars — fails decode
		{"non_hex", "z" + hex.EncodeToString(make([]byte, 32))[1:], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.RecordingConfig{LocalKEKs: map[string]string{"kek-1": tc.hexKEK}}
			_, _, err := recordingPorts(cfg, logger)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRecordingPorts_PopulatedMapBuildsLocalUnwrapper(t *testing.T) {
	t.Parallel()
	logger := zaptest.NewLogger(t)
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	cfg := config.RecordingConfig{
		LocalKEKs: map[string]string{"kek-test": hex.EncodeToString(kek)},
	}
	kms, objects, err := recordingPorts(cfg, logger)
	require.NoError(t, err)
	require.NotNil(t, kms)
	require.NotNil(t, objects)
}

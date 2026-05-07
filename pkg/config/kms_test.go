package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKMSConfig_DevDefaults(t *testing.T) {
	t.Parallel()
	c := DefaultDev()
	require.NoError(t, c.Validate())
	// Dev default uses the in-process fallback so `make dev-up` boots
	// without a real Yandex KMS endpoint.
	assert.Equal(t, KMSProviderLocal, c.KMS.Provider)
	assert.NotEmpty(t, c.KMS.LocalKeyHex,
		"dev default must seed a local key so the in-process KMS works out of the box")
}

func TestKMSConfig_Validate_RejectsUnknownProvider(t *testing.T) {
	t.Parallel()
	cfg := KMSConfig{Provider: "aws"}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider")
}

func TestKMSConfig_Validate_AcceptsLocal(t *testing.T) {
	t.Parallel()
	cfg := KMSConfig{
		Provider:    "local",
		LocalKeyHex: strings.Repeat("a", 64), // 32 bytes hex-encoded
	}
	require.NoError(t, cfg.Validate())
}

func TestKMSConfig_Validate_LocalRequiresKey(t *testing.T) {
	t.Parallel()
	cfg := KMSConfig{Provider: "local"}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local_key_hex")
}

func TestKMSConfig_Validate_AcceptsYandex(t *testing.T) {
	t.Parallel()
	cfg := KMSConfig{
		Provider:              "yandex",
		Endpoint:              "kms.api.cloud.yandex.net:443",
		FolderID:              "b1g0...",
		ServiceAccountKeyPath: "/var/run/secrets/yc-kms/sa-key.json",
	}
	require.NoError(t, cfg.Validate())
}

func TestKMSConfig_Validate_YandexRequiresFolder(t *testing.T) {
	t.Parallel()
	cfg := KMSConfig{
		Provider:              "yandex",
		Endpoint:              "kms.api.cloud.yandex.net:443",
		ServiceAccountKeyPath: "/key.json",
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "folder_id")
}

func TestKMSConfig_Validate_DefaultsProviderToLocal(t *testing.T) {
	t.Parallel()
	// Empty provider falls back to "local" (the dev default), so the
	// zero-value KMSConfig is treated as a request for the in-process
	// fallback. This keeps tests and `make dev-up` ergonomic.
	cfg := KMSConfig{LocalKeyHex: strings.Repeat("b", 64)}
	require.NoError(t, cfg.Validate())
}

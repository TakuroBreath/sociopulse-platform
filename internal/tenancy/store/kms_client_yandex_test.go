package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/tenancy/store"
)

// The Yandex KMS adapter is gated behind the `yandex_kms` build tag —
// the default-build stub in store/kms_client_yandex.go returns an error
// from the constructor so operators learn immediately that the SDK was
// not compiled in. These tests exercise the stub's contract surface:
// config validation and the explanatory constructor error.
//
// The real adapter and its smoke test live in store/kms_client_yandex_real.go
// (build tag `yandex_kms` + `integration`). Running them requires:
//   - YANDEX_KMS_ENDPOINT, YANDEX_KMS_FOLDER_ID, YANDEX_KMS_SA_KEY_PATH
//     environment variables pointing at a real Yandex Cloud KMS
//     test folder.
//   - go test -tags 'integration yandex_kms' ./internal/tenancy/store/...
//
// In CI we run only the default build; the real adapter is exercised
// in the operator's local environment before a release rotation.

func TestYandexKMSConfig_Validate_RejectsEmptyEndpoint(t *testing.T) {
	t.Parallel()
	cfg := store.YandexKMSConfig{
		FolderID:              "f1",
		ServiceAccountKeyPath: "/tmp/key.json",
	}
	require.Error(t, cfg.Validate())
}

func TestYandexKMSConfig_Validate_RejectsEmptyFolder(t *testing.T) {
	t.Parallel()
	cfg := store.YandexKMSConfig{
		Endpoint:              "kms.api.cloud.yandex.net:443",
		ServiceAccountKeyPath: "/tmp/key.json",
	}
	require.Error(t, cfg.Validate())
}

func TestYandexKMSConfig_Validate_RejectsEmptyKeyPath(t *testing.T) {
	t.Parallel()
	cfg := store.YandexKMSConfig{
		Endpoint: "kms.api.cloud.yandex.net:443",
		FolderID: "f1",
	}
	require.Error(t, cfg.Validate())
}

func TestYandexKMSConfig_Validate_AcceptsCompleteConfig(t *testing.T) {
	t.Parallel()
	cfg := store.YandexKMSConfig{
		Endpoint:              "kms.api.cloud.yandex.net:443",
		FolderID:              "b1g0w0...",
		ServiceAccountKeyPath: "/var/run/secrets/yc-kms/sa-key.json",
	}
	require.NoError(t, cfg.Validate())
}

func TestNewYandexKMSClient_DefaultBuildExplains(t *testing.T) {
	t.Parallel()
	// In the default build, NewYandexKMSClient must return an error
	// pointing operators at the right escape hatch (rebuild with the
	// build tag, or switch to provider=local).
	_, err := store.NewYandexKMSClient(context.Background(), store.YandexKMSConfig{
		Endpoint:              "kms.api.cloud.yandex.net:443",
		FolderID:              "f1",
		ServiceAccountKeyPath: "/tmp/key.json",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "yandex_kms",
		"error must mention the build tag so operators know how to enable the real adapter")
}

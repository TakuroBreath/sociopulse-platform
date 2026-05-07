//go:build !yandex_s3

package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/tenancy/store"
)

// TestYandexBucketConfig_Validate_ReportsMissingFields exercises the
// per-field invariants of YandexBucketConfig.Validate so a misconfigured
// production deployment surfaces the FIRST missing value.
func TestYandexBucketConfig_Validate_ReportsMissingFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     store.YandexBucketConfig
		wantSub string
	}{
		{
			name:    "missing endpoint",
			cfg:     store.YandexBucketConfig{},
			wantSub: "endpoint",
		},
		{
			name:    "missing region",
			cfg:     store.YandexBucketConfig{Endpoint: "storage.yandexcloud.net"},
			wantSub: "region",
		},
		{
			name: "missing access key",
			cfg: store.YandexBucketConfig{
				Endpoint: "storage.yandexcloud.net",
				Region:   "ru-central1",
			},
			wantSub: "access_key_id",
		},
		{
			name: "missing secret",
			cfg: store.YandexBucketConfig{
				Endpoint:    "storage.yandexcloud.net",
				Region:      "ru-central1",
				AccessKeyID: "ak",
			},
			wantSub: "secret_access_key",
		},
		{
			name: "valid",
			cfg: store.YandexBucketConfig{
				Endpoint:        "storage.yandexcloud.net",
				Region:          "ru-central1",
				AccessKeyID:     "ak",
				SecretAccessKey: "sk",
			},
			wantSub: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantSub == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestNewYandexBucketProvisioner_StubReturnsExplanatoryError documents the
// default-build stub: building without `-tags=yandex_s3` produces a clear
// error that points operators at the escape hatch.
func TestNewYandexBucketProvisioner_StubReturnsExplanatoryError(t *testing.T) {
	t.Parallel()
	cfg := store.YandexBucketConfig{
		Endpoint:        "storage.yandexcloud.net",
		Region:          "ru-central1",
		AccessKeyID:     "ak",
		SecretAccessKey: "sk",
	}
	p, err := store.NewYandexBucketProvisioner(context.Background(), cfg)
	require.Error(t, err)
	require.Nil(t, p)
	require.Contains(t, err.Error(), "yandex_s3",
		"stub error must surface the build tag operators rebuild with")
}

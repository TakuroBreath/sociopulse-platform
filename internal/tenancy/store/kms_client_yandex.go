// kms_client_yandex.go is the default-build stub for the Yandex Cloud KMS
// adapter. The real SDK-backed implementation lives in
// kms_client_yandex_real.go behind the `yandex_kms` build tag.
//
// Why two files: the Yandex SDK is heavyweight (transitively pulls in
// yandex-cloud/go-genproto, gRPC stubs for every Yandex service, etc.).
// Pulling it into every developer's go.sum slows builds and increases
// the supply-chain surface for a feature that only production needs.
// The stub keeps `go build ./...` cheap and dependency-free for unit
// tests; CI / production builds enable `-tags=yandex_kms` to compile
// the real adapter.
//
// This file is also subject to the depguard `yandex-sdk-isolation` rule
// — the rule allows the SDK only in `internal/tenancy/store/**`,
// `cmd/recording-uploader/**`, and `cmd/api/main.go`. Both files in
// this directory satisfy the rule.

//go:build !yandex_kms

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

// YandexKMSConfig captures the runtime parameters needed by the Yandex
// SDK. Mirrors the YAML under `kms.*`. The same struct is consumed by
// the real adapter under build tag `yandex_kms`.
type YandexKMSConfig struct {
	// Endpoint is the Yandex KMS gRPC endpoint, default
	// "kms.api.cloud.yandex.net:443".
	Endpoint string
	// FolderID is the Yandex Cloud folder where per-tenant KEKs are
	// created.
	FolderID string
	// ServiceAccountKeyPath is the filesystem path to the IAM SA key
	// JSON used to authenticate to KMS. Mounted by the Lockbox CSI
	// driver in production.
	ServiceAccountKeyPath string
}

// Validate enforces required fields.
func (c YandexKMSConfig) Validate() error {
	if c.Endpoint == "" {
		return errors.New("yandex kms: endpoint required")
	}
	if c.FolderID == "" {
		return errors.New("yandex kms: folder_id required")
	}
	if c.ServiceAccountKeyPath == "" {
		return errors.New("yandex kms: service_account_key_path required")
	}
	return nil
}

// NewYandexKMSClient is the build-tag-gated constructor for the Yandex
// KMS adapter. This stub returns an explanatory error so the operator
// learns immediately that the binary was built without the SDK.
//
// To enable: rebuild with `-tags=yandex_kms` (and ensure the Yandex
// SDK is on the module path).
func NewYandexKMSClient(_ context.Context, _ YandexKMSConfig) (api.KMSClient, error) {
	return nil, fmt.Errorf(
		"yandex kms: SDK not compiled in — rebuild with `-tags=yandex_kms` to enable, " +
			"or set `kms.provider: local` for the in-process dev fallback")
}

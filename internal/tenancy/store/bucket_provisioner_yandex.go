// bucket_provisioner_yandex.go is the default-build stub for the Yandex
// Object Storage adapter. The real S3-SDK-backed implementation lives in
// bucket_provisioner_yandex_real.go behind the `yandex_s3` build tag.
//
// Why two files: pulling the AWS SDK v2 (which is what every realistic
// Yandex Object Storage client uses, since YOS speaks the S3 protocol) into
// every developer's go.sum slows builds and inflates the supply-chain
// surface for a feature only production needs. The stub keeps
// `go build ./...` cheap and dependency-free for unit tests; CI / production
// builds enable `-tags=yandex_s3` to compile the real adapter.
//
// This file is also subject to the depguard `yandex-sdk-isolation` rule
// — the rule allows the SDK only in `internal/tenancy/store/**`,
// `cmd/recording-uploader/**`, and `cmd/api/main.go`. Both files in this
// package satisfy the rule.

//go:build !yandex_s3

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

// YandexBucketConfig captures the runtime parameters needed by the Yandex
// Object Storage adapter. Mirrors the YAML under `s3.*`. The same struct is
// consumed by the real adapter under build tag `yandex_s3`.
type YandexBucketConfig struct {
	// Endpoint is the Yandex Object Storage endpoint, e.g.
	// "storage.yandexcloud.net".
	Endpoint string
	// Region is the bucket region, e.g. "ru-central1".
	Region string
	// AccessKeyID / SecretAccessKey are the static credentials. Mounted via
	// Lockbox CSI in production; never logged.
	AccessKeyID     string
	SecretAccessKey string
	// BucketPrefix is the deterministic prefix prepended to tenant IDs to
	// form a bucket name. Defaults to "sociopulse-recordings-" if empty.
	BucketPrefix string
}

// Validate enforces required fields.
func (c YandexBucketConfig) Validate() error {
	if c.Endpoint == "" {
		return errors.New("yandex s3: endpoint required")
	}
	if c.Region == "" {
		return errors.New("yandex s3: region required")
	}
	if c.AccessKeyID == "" {
		return errors.New("yandex s3: access_key_id required")
	}
	if c.SecretAccessKey == "" {
		return errors.New("yandex s3: secret_access_key required")
	}
	return nil
}

// yandexBucketProvisionerStub is the default-build placeholder. Every
// method returns an error pointing operators at the build-tag escape hatch.
type yandexBucketProvisionerStub struct{}

// Compile-time check: the stub satisfies api.BucketProvisioner so the
// production wiring path type-checks even when the SDK is not compiled in.
var _ api.BucketProvisioner = (*yandexBucketProvisionerStub)(nil)

// NewYandexBucketProvisioner is the build-tag-gated constructor for the
// Yandex Object Storage adapter. The default-build stub returns an
// explanatory error so the operator learns immediately that the binary was
// built without the S3 SDK.
//
// To enable: rebuild with `-tags=yandex_s3` (and ensure the AWS SDK v2 is
// on the module path).
func NewYandexBucketProvisioner(_ context.Context, _ YandexBucketConfig) (api.BucketProvisioner, error) {
	return nil, fmt.Errorf(
		"yandex s3: SDK not compiled in — rebuild with `-tags=yandex_s3` to enable, " +
			"or set `s3.provider: local` for the in-process dev fallback")
}

// Provision returns a clear error matching the constructor — defensive
// belt-and-braces in case the stub is wired through some other path.
func (*yandexBucketProvisionerStub) Provision(_ context.Context, _ uuid.UUID, _ string) (string, error) {
	return "", fmt.Errorf("%w: yandex s3 SDK not compiled in (rebuild with -tags=yandex_s3)",
		api.ErrBucketProvisionFailed)
}

// Decommission returns a clear error matching the constructor.
func (*yandexBucketProvisionerStub) Decommission(_ context.Context, _ uuid.UUID) error {
	return fmt.Errorf("%w: yandex s3 SDK not compiled in (rebuild with -tags=yandex_s3)",
		api.ErrBucketProvisionFailed)
}

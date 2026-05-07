package api

import (
	"context"
	"io"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Tenancy is the aggregate interface exposed to other modules. It bundles
// every public method of the tenancy module so that downstream code accepts
// a single dependency.
//
// Direct embedding requires the four sub-interfaces' method sets to be
// disjoint: TenantService.Get and SettingsCache's lookups would collide on
// "Get" — so SettingsCache uses Lookup/LookupWithDefault/LookupAll instead.
type Tenancy interface {
	TenantService
	SettingsCache
	KMSResolver
	PhoneHasher
}

// Deps is the dependency bundle that Module.Register requires.
//
// All fields are required unless marked otherwise. The caller (cmd/api/main.go)
// owns the lifecycle of every dependency.
type Deps struct {
	// Logger — module-scoped child logger.
	Logger *zap.Logger

	// Pool — pkg/postgres pool wrapper. Cross-tenant CRUD goes through
	// Pool.BypassRLS (tenancy_admin role); per-tenant reads use the normal
	// SET LOCAL app.tenant_id path.
	Pool *postgres.Pool

	// EventBus — publisher used for cache invalidation and lifecycle events
	// (tenant.<id>.created, tenant.<id>.suspended, tenant.<id>.archived,
	// tenant.<id>.settings.updated).
	EventBus eventbus.Publisher

	// Subscriber — used to listen for peer cache-invalidation messages.
	Subscriber eventbus.Subscriber

	// KMS — Yandex KMS client. Wraps yandex-cloud/go-sdk.
	KMS KMSClient

	// Config — module configuration parsed from config.yaml under `tenancy:`.
	Config Config
}

// Config mirrors the `tenancy:` block in config.yaml. See spec §14.2 / §14.4.
type Config struct {
	// DEKCacheTTL — how long a per-tenant DEK lives in process memory.
	// Default 5m. Spec §6.2.
	DEKCacheTTL string `yaml:"dek_cache_ttl"`

	// DEKCacheSize — max distinct tenants cached. Default 1024.
	DEKCacheSize int `yaml:"dek_cache_size"`

	// SettingsCacheTTL — how long a setting value lives in process memory.
	// Default 30s. Spec §14.1.
	SettingsCacheTTL string `yaml:"settings_cache_ttl"`

	// SettingsCacheSize — max distinct (tenantID, key) entries. Default 65536.
	SettingsCacheSize int `yaml:"settings_cache_size"`

	// KMSEndpoint — Yandex KMS gRPC endpoint, default "kms.api.cloud.yandex.net:443".
	KMSEndpoint string `yaml:"kms_endpoint"`

	// KMSFolderID — folder ID where per-tenant KEKs are created.
	KMSFolderID string `yaml:"kms_folder_id"`

	// KMSServiceAccountKeyPath — path to the IAM SA key JSON used to auth into KMS.
	// Mounted by Lockbox CSI driver; loaded once at module init.
	KMSServiceAccountKeyPath string `yaml:"kms_service_account_key_path"`
}

// KMSClient is the abstraction over yandex-cloud/go-sdk that the kmsResolver
// service uses. We wrap the SDK behind this interface so that:
//  1. Unit tests inject a fake (no network).
//  2. Integration tests can swap in a local fake KMS server.
//  3. The yandex-cloud SDK upgrade does not ripple into business logic.
type KMSClient interface {
	// CreateKey creates a new per-tenant symmetric KEK in the configured folder.
	CreateKey(ctx context.Context, name, description string) (keyID string, err error)

	// Encrypt wraps a plaintext data key using the given KEK. Returns ciphertext
	// + the KEK version that wrapped it.
	Encrypt(ctx context.Context, keyID string, plaintext []byte) (ciphertext []byte, version string, err error)

	// Decrypt unwraps a ciphertext data key. Returns the plaintext + the KEK
	// version that originally wrapped it.
	Decrypt(ctx context.Context, keyID string, ciphertext []byte) (plaintext []byte, version string, err error)

	// GenerateDataKey is the single-call envelope op. Equivalent to: Encrypt(rand.Read(32))
	// but performed atomically by KMS.
	GenerateDataKey(ctx context.Context, keyID string) (plaintextDEK, ciphertextDEK []byte, version string, err error)
}

// Module is the top-level handle for the tenancy module.
type Module struct {
	deps    Deps
	tenancy Tenancy
	closer  io.Closer
}

// NewModule constructs a Module from already-wired dependencies. Use this
// when an integrator has built the four sub-interfaces by hand (e.g. during
// tests). Production callers go through Register, which performs the full
// store/service composition.
func NewModule(deps Deps, tenancy Tenancy, closer io.Closer) *Module {
	return &Module{deps: deps, tenancy: tenancy, closer: closer}
}

// Tenancy returns the aggregate interface. Safe to call after Register
// returns no error.
func (m *Module) Tenancy() Tenancy { return m.tenancy }

// Deps returns the dependency bundle the module was constructed with.
// Useful in tests and at shutdown.
func (m *Module) Deps() Deps { return m.deps }

// Stop releases resources (cache, NATS subscriptions). Idempotent.
func (m *Module) Stop() error {
	if m.closer == nil {
		return nil
	}
	return m.closer.Close()
}

// Register is the seam through which the service package supplies its
// implementation. api/ never imports service/; instead service/register.go
// (Plan 04 Task 2) sets this variable in an init() so cmd/api can call
// api.Register without coupling api/ to service/.
//
// Until Task 2 lands, Register is nil. cmd/api guards the nil case with
// a clear error.
var Register func(ctx context.Context, deps Deps) (*Module, error)

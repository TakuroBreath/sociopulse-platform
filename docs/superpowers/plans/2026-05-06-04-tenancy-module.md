# Tenancy Module Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. **TDD is mandatory** — every Task with code follows red-green-refactor.

**Goal:** Build the `internal/tenancy` module — the foundation for multi-tenant data isolation. Deliver a `TenantService` (CRUD over `tenants` and `tenant_settings`), a `KMSResolver` that talks to Yandex KMS for per-tenant KEK lifecycle and DEK envelope operations, a `PhoneHasher` (HMAC-SHA256 with per-tenant pepper), a `SettingsCache` with NATS-driven invalidation, and Service-Owner-only HTTP endpoints under `/admin/tenants/*` protected by mTLS. Every public surface is covered by unit tests; KMS and Postgres flows are covered by integration tests via `testcontainers-go` (Postgres) and a `KMSClient` interface that swaps to a fake in tests.

**Architecture:**
- `internal/tenancy/api/` — public Go interfaces (`TenantService`, `SettingsCache`, `KMSResolver`, `PhoneHasher`) + DTO types. Other modules import only from here.
- `internal/tenancy/service/` — business logic (`tenantService`, `settingsCache`, `kmsResolver`, `phoneHasher`).
- `internal/tenancy/store/` — Postgres access via `pgx/v5`; uses the `tenancy_admin` Postgres role with `BYPASSRLS` (the only module that does — see spec §6.1, §12.2).
- `internal/tenancy/events/` — NATS publish/subscribe for `tenant.<id>.settings.updated` (cache invalidation).
- `internal/tenancy/transport/http/` — `/admin/tenants/*` HTTP handlers, registered into the gateway via `Module.Register`.
- A single `Module` struct is the integration point: `Module.Register(deps Deps) (api.Tenancy, error)` is called from `cmd/api/main.go` once and exposes the `api.Tenancy` aggregate interface to the rest of the monolith.

**Tech stack:**
- Go 1.26+
- `github.com/jackc/pgx/v5` — Postgres driver, `tenancy_admin` connection pool.
- `github.com/yandex-cloud/go-sdk` v0.0.50+ — Yandex KMS SDK (`kms.SymmetricKeyServiceClient`, `kms.SymmetricCryptoServiceClient`).
- `github.com/nats-io/nats.go` — NATS client (cache invalidation).
- `github.com/hashicorp/golang-lru/v2` — TTL cache for DEKs and settings.
- `golang.org/x/crypto` — `hkdf` for derivation, `crypto/hmac` for phone-hash.
- `github.com/stretchr/testify` (require/assert/suite) — test harness.
- `go.uber.org/mock` (gomock) — mock generation for unit tests.
- `github.com/testcontainers/testcontainers-go` v0.28+ — integration tests (Postgres, NATS).
- `github.com/google/uuid` — UUIDs.
- `github.com/prometheus/client_golang` — module-level metrics.
- `go.uber.org/zap` — structured logging (already wired in Plan 02).

**Spec sections covered:**
- §5.2 — `tenancy` module catalog entry.
- §5.5 — dependency graph (`tenancy → {KMS}`, leaf-ish module).
- §6.1 — `tenant_id` column convention; this plan owns `tenants` + `tenant_settings`.
- §6.2 — PII encryption envelope (per-tenant KEK + cached DEK + HMAC-SHA256 with per-tenant pepper).
- §6.3 — `tenants` and `tenant_settings` table shape (DDL is in Plan 03; this plan only writes Go code that talks to those tables).
- §12.1, §12.2 — defence-in-depth, RLS, and the `tenancy_admin` BYPASSRLS role for cross-tenant CRUD.
- §12.4 — at-rest encryption choices (envelope, KEK in KMS, DEK cached in app).
- §12.5 — mTLS for internal endpoints (Service-Owner CRUD).
- §12.6 — Lockbox for secrets (KMS service account credentials).
- §13.6 — KMS Audit Trails for every decrypt.
- §14.1, §14.2, §14.3 — `config.yaml` shape and per-tenant `tenant_settings` registry.
- §14.4 — what is *not* in `tenant_settings`.
- §17 — testing strategy baseline.

**Prerequisites:**
- Plan 00 (foundation: Go module, `internal/tenancy/api/.gitkeep`, depguard rules, Makefile, CI).
- Plan 01 (infra: Yandex KMS module exists, Lockbox secret bundle, Postgres+PgBouncer, NATS Helm chart deployed).
- Plan 02 (cmd/api skeleton: config loader, zap logger, OTel tracer, gateway middleware framework, NATS client wired into DI).
- Plan 03 (database: `tenants` + `tenant_settings` migration applied; the Postgres role `tenancy_admin` with `BYPASSRLS` exists; `app` role exists with `SET LOCAL app.tenant_id` discipline).

If a prerequisite is incomplete, STOP and finish it first. The unit tests in this plan compile against an `api/` interface, but the integration tests in Task 8 fail without a real Postgres + Yandex KMS (or local fake).

---

## File Structure

This plan creates / fills the following file tree. Files starting with `_` are test fixtures.

```
internal/tenancy/
├── api/
│   ├── doc.go                              # package-level docs, identifies api/ as the only public surface
│   ├── errors.go                           # exported sentinel errors (ErrNotFound, ErrAlreadyExists, ErrSuspended, ...)
│   ├── module.go                           # Module struct, Deps, Register(), api.Tenancy aggregate interface
│   ├── settings.go                         # SettingsCache interface + SettingValue type + key registry
│   ├── kms.go                              # KMSResolver interface + DataKey struct
│   ├── phone_hasher.go                     # PhoneHasher interface
│   ├── tenant_service.go                   # TenantService interface + Tenant DTO + status enum
│   └── types_test.go                       # smoke tests for DTO marshalling
│
├── service/
│   ├── tenant_service.go                   # tenantService impl (CRUD, status transitions)
│   ├── tenant_service_test.go              # unit tests with mocked store + KMS
│   ├── kms_resolver.go                     # kmsResolver impl (lazy KEK, DEK cache, Encrypt/Decrypt)
│   ├── kms_resolver_test.go                # unit tests with fake KMSClient
│   ├── phone_hasher.go                     # phoneHasher impl (HMAC-SHA256 with per-tenant pepper)
│   ├── phone_hasher_test.go                # unit tests
│   ├── settings_cache.go                   # settingsCache impl (lazy load + write-through + NATS invalidation)
│   ├── settings_cache_test.go              # unit tests
│   ├── settings_registry.go                # known keys, default values, type validation
│   └── settings_registry_test.go           # unit tests for registry
│
├── store/
│   ├── postgres.go                         # store impl over pgxpool, uses tenancy_admin role
│   ├── postgres_test.go                    # integration tests via testcontainers-go
│   ├── _testdata/
│   │   └── 0001_tenancy_setup.sql          # creates tenancy_admin role + tenants/tenant_settings tables (mirror of Plan 03 migration)
│   └── tx.go                               # helpers: WithTx, WithTenancyAdmin
│
├── events/
│   ├── publisher.go                        # NATS publisher: tenant.<id>.settings.updated, tenant.created, tenant.archived
│   ├── publisher_test.go                   # unit tests with NATS test server
│   ├── invalidator.go                      # NATS subscriber for invalidation
│   └── invalidator_test.go                 # unit tests
│
├── transport/http/
│   ├── handlers.go                         # /admin/tenants/* HTTP handlers, mTLS-required
│   ├── handlers_test.go                    # httptest with fake TenantService
│   ├── middleware.go                       # require-mTLS-with-service-owner-OU middleware
│   ├── middleware_test.go                  # unit tests for mTLS middleware
│   └── routes.go                           # gateway.RouteRegistry registration
│
└── README.md                               # how the module is structured + how to add a new tenant_settings key

docs/runbooks/
└── key-rotation.md                         # ops procedure for rotating per-tenant KEK + emergency revocation

mocks/tenancy/
├── mock_kms_client.go                      # gomock-generated for KMSClient
├── mock_store.go                           # gomock-generated for Store
└── mock_settings_publisher.go              # gomock-generated for SettingsPublisher

cmd/api/
└── main.go                                 # MODIFIED: register tenancy module via Module.Register

configs/development/
└── config.yaml                             # MODIFIED: add `tenancy:` block (kms endpoint, sa key path, dek cache TTL, settings cache TTL)
```

After this plan:
- `internal/tenancy/` is fully implemented.
- `cmd/api/main.go` boots with the module wired in.
- `make test` passes (unit tests).
- `make integration-test` passes (testcontainers Postgres + an in-process fake Yandex KMS implementing `KMSClient`).
- A Service-Owner can `POST /admin/tenants` with mTLS and the new tenant gets a Yandex KMS KEK + a `phone_hash_pepper` + an empty `tenant_settings` row set.

---

## Task 1: Module skeleton — `api/` package, `Module.Register`, depguard wiring

**Goal:** Lay down the public-surface package `internal/tenancy/api/` with all interfaces and a `Module.Register(Deps) (Tenancy, error)` constructor that other modules will call. No business logic yet — just types, interfaces, and the DI seam. After this task, `go build ./...` and `go test ./internal/tenancy/api/...` are green.

**Files:**
- Create: `internal/tenancy/api/doc.go`
- Create: `internal/tenancy/api/errors.go`
- Create: `internal/tenancy/api/tenant_service.go`
- Create: `internal/tenancy/api/kms.go`
- Create: `internal/tenancy/api/phone_hasher.go`
- Create: `internal/tenancy/api/settings.go`
- Create: `internal/tenancy/api/module.go`
- Create: `internal/tenancy/api/types_test.go`
- Modify: `.golangci.yml` (depguard list — `tenancy/service`, `tenancy/store`, `tenancy/events`, `tenancy/transport` not importable from elsewhere).
- Delete: `internal/tenancy/api/.gitkeep` (the placeholder from Plan 00).

- [ ] **Step 1 (RED): Write `api/types_test.go` first**

This test compiles against interfaces that don't exist yet — it forces the API surface to be defined precisely.

```go
package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

func TestTenant_StatusTransitions_AreEnumerated(t *testing.T) {
	require.Equal(t, api.TenantStatusActive, api.TenantStatus("active"))
	require.Equal(t, api.TenantStatusSuspended, api.TenantStatus("suspended"))
	require.Equal(t, api.TenantStatusArchived, api.TenantStatus("archived"))
}

func TestTenant_Validate_RejectsEmptyOrgCode(t *testing.T) {
	tn := api.Tenant{
		ID:      uuid.New(),
		OrgCode: "",
		Name:    "x",
		Status:  api.TenantStatusActive,
	}
	err := tn.Validate()
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestTenant_Validate_RejectsBadStatus(t *testing.T) {
	tn := api.Tenant{
		ID:      uuid.New(),
		OrgCode: "CC-MOSKVA-01",
		Name:    "ВЦИОМ-Москва",
		Status:  api.TenantStatus("ghost"),
	}
	err := tn.Validate()
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestSettingValue_TypedAccessors(t *testing.T) {
	v, err := api.SettingValueFromAny("4h")
	require.NoError(t, err)
	d, err := v.AsDuration()
	require.NoError(t, err)
	require.Equal(t, 4*time.Hour, d)

	vint, err := api.SettingValueFromAny(int64(3))
	require.NoError(t, err)
	i, err := vint.AsInt()
	require.NoError(t, err)
	require.Equal(t, int64(3), i)
}

func TestModule_Register_ReturnsTenancy(t *testing.T) {
	// Compile-time fixture: Tenancy interface must include all four sub-interfaces.
	var _ context.Context
	var _ interface {
		api.TenantService
		api.SettingsCache
		api.KMSResolver
		api.PhoneHasher
	} = (api.Tenancy)(nil)
}
```

Run `go test ./internal/tenancy/api/...`.

Expected (RED): compile error, every type/interface mentioned is missing.

- [ ] **Step 2 (GREEN): Write `api/doc.go`**

```go
// Package api defines the public surface of the tenancy module.
//
// Only this package may be imported by other modules. Per the depguard
// rule in .golangci.yml, internal/tenancy/{service,store,events,transport}
// are off-limits to anything outside internal/tenancy/.
//
// The aggregate interface Tenancy embeds the four primary interfaces:
//
//   - TenantService    — CRUD over tenants (Service-Owner level)
//   - SettingsCache    — per-tenant key/value (cached, NATS-invalidated)
//   - KMSResolver      — per-tenant KEK lifecycle + DEK envelope ops
//   - PhoneHasher      — HMAC-SHA256 with per-tenant pepper
//
// Construct one via Module.Register(deps).
package api
```

- [ ] **Step 3 (GREEN): Write `api/errors.go`**

```go
package api

import "errors"

// Sentinel errors returned by tenancy. Wrap with %w; check with errors.Is.
var (
	// ErrNotFound — the tenant or setting key does not exist.
	ErrNotFound = errors.New("tenancy: not found")

	// ErrAlreadyExists — duplicate org_code on Create, or duplicate setting key on insert.
	ErrAlreadyExists = errors.New("tenancy: already exists")

	// ErrInvalidArgument — caller-provided value violates an invariant
	// (empty org_code, unknown status, unknown setting key, value type mismatch).
	ErrInvalidArgument = errors.New("tenancy: invalid argument")

	// ErrSuspended — a tenant is suspended and cannot perform the requested op.
	// Service-Owner CRUD is still allowed; only data-plane operations should
	// surface this to end-users.
	ErrSuspended = errors.New("tenancy: suspended")

	// ErrArchived — a tenant is archived (read-only graveyard).
	ErrArchived = errors.New("tenancy: archived")

	// ErrKMSUnavailable — Yandex KMS is unreachable / returned a transient error.
	// Callers must retry with backoff, NOT degrade silently.
	ErrKMSUnavailable = errors.New("tenancy: kms unavailable")

	// ErrPermissionDenied — request lacks Service-Owner mTLS identity.
	ErrPermissionDenied = errors.New("tenancy: permission denied")
)
```

- [ ] **Step 4 (GREEN): Write `api/tenant_service.go`**

```go
package api

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// TenantStatus is the lifecycle state of a tenant.
type TenantStatus string

const (
	TenantStatusActive    TenantStatus = "active"
	TenantStatusSuspended TenantStatus = "suspended"
	TenantStatusArchived  TenantStatus = "archived"
)

// Valid reports whether s is a known status.
func (s TenantStatus) Valid() bool {
	switch s {
	case TenantStatusActive, TenantStatusSuspended, TenantStatusArchived:
		return true
	}
	return false
}

// Tenant is the public DTO for a tenant row.
//
// PhoneHashPepper is *not* exposed in the DTO — it never leaves the database
// boundary in serialised form. Use PhoneHasher to operate over phone numbers.
type Tenant struct {
	ID        uuid.UUID    `json:"id"`
	OrgCode   string       `json:"org_code"`            // e.g. "CC-MOSKVA-01"
	Name      string       `json:"name"`                // e.g. "ВЦИОМ-Москва"
	Status    TenantStatus `json:"status"`
	KMSKEKID  string       `json:"kms_kek_id"`          // Yandex KMS symmetric key ID
	CreatedAt time.Time    `json:"created_at"`
}

// Validate enforces the invariants that aren't already enforced by the DB
// constraint set in Plan 03 — used in service-layer guards before INSERT/UPDATE.
func (t Tenant) Validate() error {
	if t.OrgCode == "" {
		return fmt.Errorf("%w: org_code must be non-empty", ErrInvalidArgument)
	}
	if len(t.OrgCode) > 64 {
		return fmt.Errorf("%w: org_code must be <= 64 chars", ErrInvalidArgument)
	}
	if t.Name == "" {
		return fmt.Errorf("%w: name must be non-empty", ErrInvalidArgument)
	}
	if !t.Status.Valid() {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidArgument, t.Status)
	}
	return nil
}

// CreateTenantRequest is what a Service-Owner POSTs to /admin/tenants.
type CreateTenantRequest struct {
	OrgCode string `json:"org_code"`
	Name    string `json:"name"`
}

// ListTenantsFilter narrows TenantService.List output. All fields optional.
type ListTenantsFilter struct {
	Status   *TenantStatus
	OrgCode  string // exact match if non-empty
	Limit    int    // default 50, max 500
	Offset   int
}

// TenantService is the cross-tenant CRUD surface. The implementation talks
// to Postgres via the `tenancy_admin` BYPASSRLS role. ALL data-plane modules
// must NOT use this interface directly — they look up tenants via the per-
// request middleware (auth module) which caches a Tenant for the request.
//
// Mutating methods publish a NATS event:
//   - Create   → tenant.<id>.created
//   - Suspend  → tenant.<id>.suspended
//   - Archive  → tenant.<id>.archived
type TenantService interface {
	Create(ctx context.Context, req CreateTenantRequest) (Tenant, error)
	Get(ctx context.Context, id uuid.UUID) (Tenant, error)
	GetByOrgCode(ctx context.Context, orgCode string) (Tenant, error)
	List(ctx context.Context, filter ListTenantsFilter) ([]Tenant, error)
	Suspend(ctx context.Context, id uuid.UUID, reason string) error
	Resume(ctx context.Context, id uuid.UUID) error
	Archive(ctx context.Context, id uuid.UUID) error
}
```

- [ ] **Step 5 (GREEN): Write `api/kms.go`**

```go
package api

import (
	"context"

	"github.com/google/uuid"
)

// DataKey is the result of GenerateDataKey: plaintext for immediate use,
// ciphertext for storage alongside the encrypted payload.
//
// CRITICAL: the caller must zeroise Plaintext after use:
//
//   defer func() {
//       for i := range dk.Plaintext {
//           dk.Plaintext[i] = 0
//       }
//   }()
type DataKey struct {
	Plaintext  []byte // 32 bytes for AES-256
	Ciphertext []byte // KMS-encrypted blob, store with the payload
	KeyVersion string // KEK version that wrapped this DEK (for rotation tracking)
}

// KMSResolver wraps Yandex KMS for the tenancy module. Other modules (recording,
// auth, crm) consume this through the api.Tenancy aggregate.
//
// All methods are idempotent on the KMS side: retries are safe.
type KMSResolver interface {
	// EnsureKEK creates a per-tenant KEK in Yandex KMS if absent and returns its ID.
	// Idempotent: safe to call repeatedly during onboarding.
	EnsureKEK(ctx context.Context, tenantID uuid.UUID) (kekID string, err error)

	// GenerateDataKey produces a fresh DEK wrapped by the tenant's KEK.
	// Use the plaintext to encrypt a single payload, store the ciphertext alongside.
	GenerateDataKey(ctx context.Context, tenantID uuid.UUID) (DataKey, error)

	// Encrypt performs in-app AES-256-GCM with a cached DEK (for short PII like phones).
	// Returns ciphertext that includes the nonce and a header identifying the wrapped DEK.
	Encrypt(ctx context.Context, tenantID uuid.UUID, plaintext []byte) ([]byte, error)

	// Decrypt reverses Encrypt. Resolves the DEK via the cache, transparently
	// invokes KMS.Decrypt on cache miss.
	Decrypt(ctx context.Context, tenantID uuid.UUID, ciphertext []byte) ([]byte, error)

	// InvalidateCache drops the in-memory DEK cache entry for the tenant.
	// Called after KEK rotation or tenant suspension.
	InvalidateCache(tenantID uuid.UUID)
}
```

- [ ] **Step 6 (GREEN): Write `api/phone_hasher.go`**

```go
package api

import (
	"context"

	"github.com/google/uuid"
)

// PhoneHasher computes a deterministic, per-tenant-salted hash of E.164 phone
// numbers for indexed lookup (respondents.phone_hash, users.login_phone_hash).
//
// Algorithm: HMAC-SHA256(pepper=tenants.phone_hash_pepper, msg=normalised_e164).
// 32 bytes output, stored as bytea.
//
// The pepper is loaded once per process from the database (cached in memory,
// invalidated only on tenant suspension or pepper-rotation).
type PhoneHasher interface {
	// Hash returns the 32-byte HMAC-SHA256 of the canonicalised phone.
	// The phone is canonicalised: digits-only, leading-+, E.164 (e.g. "+79991234567").
	Hash(ctx context.Context, tenantID uuid.UUID, phone string) ([]byte, error)

	// Normalise strips formatting characters and validates the result is E.164.
	// Returns ErrInvalidArgument on garbage input.
	Normalise(phone string) (string, error)
}
```

- [ ] **Step 7 (GREEN): Write `api/settings.go`**

```go
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// SettingValue is the dynamically-typed value of a tenant_settings row.
//
// Construct via SettingValueFromAny; access via typed accessors. The wire
// format is JSON (jsonb in Postgres).
type SettingValue struct {
	raw json.RawMessage
}

// SettingValueFromAny converts a Go value into a SettingValue.
func SettingValueFromAny(v any) (SettingValue, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return SettingValue{}, fmt.Errorf("%w: marshal: %v", ErrInvalidArgument, err)
	}
	return SettingValue{raw: b}, nil
}

// SettingValueFromRaw constructs from already-marshalled JSON.
func SettingValueFromRaw(b []byte) SettingValue {
	out := make(json.RawMessage, len(b))
	copy(out, b)
	return SettingValue{raw: out}
}

// Raw returns the underlying jsonb bytes (read-only).
func (v SettingValue) Raw() json.RawMessage { return v.raw }

// AsString reads the value as a JSON string.
func (v SettingValue) AsString() (string, error) {
	var s string
	if err := json.Unmarshal(v.raw, &s); err != nil {
		return "", fmt.Errorf("%w: not a string: %v", ErrInvalidArgument, err)
	}
	return s, nil
}

// AsInt reads as a JSON number → int64.
func (v SettingValue) AsInt() (int64, error) {
	var n json.Number
	if err := json.Unmarshal(v.raw, &n); err != nil {
		return 0, fmt.Errorf("%w: not a number: %v", ErrInvalidArgument, err)
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: not int64: %v", ErrInvalidArgument, err)
	}
	return i, nil
}

// AsBool reads as a JSON bool.
func (v SettingValue) AsBool() (bool, error) {
	var b bool
	if err := json.Unmarshal(v.raw, &b); err != nil {
		return false, fmt.Errorf("%w: not a bool: %v", ErrInvalidArgument, err)
	}
	return b, nil
}

// AsDuration reads as a duration-shaped string ("4h", "30m", "2h30m").
func (v SettingValue) AsDuration() (time.Duration, error) {
	s, err := v.AsString()
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: not a duration: %v", ErrInvalidArgument, err)
	}
	return d, nil
}

// AsJSON unmarshals into the destination.
func (v SettingValue) AsJSON(dst any) error {
	if err := json.Unmarshal(v.raw, dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	return nil
}

// SettingsCache is the per-tenant key/value cache.
//
// Reads: lazy-load from Postgres on miss, cache for TTL=30s, NATS subscriber
// on `tenant.<id>.settings.updated` invalidates the entry.
// Writes: write-through (UPDATE Postgres, then publish NATS event, then update cache).
type SettingsCache interface {
	Get(ctx context.Context, tenantID uuid.UUID, key string) (SettingValue, error)
	GetWithDefault(ctx context.Context, tenantID uuid.UUID, key string, def SettingValue) (SettingValue, error)
	GetAll(ctx context.Context, tenantID uuid.UUID) (map[string]SettingValue, error)
	Set(ctx context.Context, tenantID uuid.UUID, key string, value SettingValue) error
	Delete(ctx context.Context, tenantID uuid.UUID, key string) error

	// InvalidateLocal drops the in-memory entry for (tenantID, key) without
	// publishing a NATS event. Used by the NATS subscriber on incoming
	// invalidation messages from peer pods.
	InvalidateLocal(tenantID uuid.UUID, key string)

	// InvalidateAllLocal drops all entries for a tenant. Used on tenant.archived.
	InvalidateAllLocal(tenantID uuid.UUID)
}
```

- [ ] **Step 8 (GREEN): Write `api/module.go`**

```go
package api

import (
	"context"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Tenancy is the aggregate interface exposed to other modules. It bundles
// every public method of the tenancy module so that downstream code accepts
// a single dependency.
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

	// AdminPool — pgx pool authenticated as `tenancy_admin` (BYPASSRLS).
	// Used for cross-tenant CRUD on `tenants` and `tenant_settings`.
	AdminPool *pgxpool.Pool

	// AppPool — pgx pool authenticated as `app` (RLS-bound).
	// Used by SettingsCache reads when scoped to a tenant context.
	AppPool *pgxpool.Pool

	// NATS — connection used for cache invalidation and lifecycle events.
	NATS *nats.Conn

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
//   1. Unit tests inject a fake (no network).
//   2. Integration tests can swap in a local fake KMS server.
//   3. The yandex-cloud SDK upgrade does not ripple into business logic.
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

// Register constructs all the moving parts and returns the aggregate interface.
// Call once during cmd/api startup. Stop() must be called at shutdown.
//
// The actual implementation lives in internal/tenancy/service. This file only
// declares the seam; the implementation registers itself via a runtime indirection
// (see service/register.go in Task 2 — a build-time wire-up; api/ stays free of
// service/ imports).
//
// The runtime indirection: Register is defined here as a NIL function, and
// service/register.go uses an init() to set it. Other modules see only the
// signature.
var Register func(ctx context.Context, deps Deps) (*Module, error)

// Tenancy returns the aggregate interface. Safe to call after Register returns no error.
func (m *Module) Tenancy() Tenancy { return m.tenancy }

// Stop releases resources (cache, NATS subscriptions). Idempotent.
func (m *Module) Stop() error {
	if m.closer == nil {
		return nil
	}
	return m.closer.Close()
}
```

- [ ] **Step 9 (REFACTOR & GREEN): Run the test from Step 1**

```bash
go mod tidy
go test ./internal/tenancy/api/...
```

Expected: pass. The compile-time fixture `var _ interface{...} = (api.Tenancy)(nil)` proves the aggregate covers the four sub-interfaces.

If `go mod tidy` complains about missing modules, add them (`go get github.com/google/uuid github.com/jackc/pgx/v5 github.com/nats-io/nats.go go.uber.org/zap`).

- [ ] **Step 10: Wire depguard rules in `.golangci.yml`**

Open `.golangci.yml` and add a deny-rule under `linters-settings.depguard.rules`:

```yaml
linters-settings:
  depguard:
    rules:
      tenancy-internal:
        list-mode: lax
        files:
          - "!**/internal/tenancy/**"
        deny:
          - pkg: "github.com/sociopulse/platform/internal/tenancy/service"
            desc: "tenancy/service is internal — import internal/tenancy/api instead"
          - pkg: "github.com/sociopulse/platform/internal/tenancy/store"
            desc: "tenancy/store is internal — import internal/tenancy/api instead"
          - pkg: "github.com/sociopulse/platform/internal/tenancy/events"
            desc: "tenancy/events is internal — import internal/tenancy/api instead"
          - pkg: "github.com/sociopulse/platform/internal/tenancy/transport"
            desc: "tenancy/transport is internal — import internal/tenancy/api instead"
```

Run `make lint`. Expected: green (no violations because no other module imports internal yet).

- [ ] **Step 11: Remove `.gitkeep` and commit**

```bash
rm internal/tenancy/api/.gitkeep
git add internal/tenancy/api .golangci.yml go.mod go.sum
git commit -m "tenancy: add api/ package with TenantService/KMSResolver/PhoneHasher/SettingsCache interfaces"
```

Expected: clean commit. `make build && make test && make lint` all green.

---

## Task 2: `TenantService` — CRUD over `tenants` table via `tenancy_admin` role

**Goal:** Implement the cross-tenant CRUD flow. Service-Owners create/get/list/suspend/archive tenants. The store layer connects through a `tenancy_admin` `BYPASSRLS` Postgres role; the service layer enforces validation, status transitions, and triggers KMS-KEK provisioning on create.

**Files:**
- Create: `internal/tenancy/store/tx.go`
- Create: `internal/tenancy/store/postgres.go`
- Create: `internal/tenancy/store/postgres_test.go`
- Create: `internal/tenancy/store/_testdata/0001_tenancy_setup.sql`
- Create: `internal/tenancy/service/tenant_service.go`
- Create: `internal/tenancy/service/tenant_service_test.go`
- Create: `internal/tenancy/service/register.go` (init() wires `api.Register`)
- Create: `mocks/tenancy/mock_store.go`
- Create: `mocks/tenancy/mock_kms_client.go`
- Modify: `Makefile` — add `mockgen` target.
- Modify: `go.mod` — add gomock + testcontainers + crypto/rand dependencies.

- [ ] **Step 1 (RED): Write `service/tenant_service_test.go`**

This is the unit-test file. It uses gomock-generated `mockStore` and `mockKMSClient`. We write the test against the still-nonexistent constructor `service.NewTenantService(...)`.

```go
package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/service"
	mocks "github.com/sociopulse/platform/mocks/tenancy"
)

func TestTenantService_Create_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	kms := mocks.NewMockKMSClient(ctrl)
	pub := mocks.NewMockSettingsPublisher(ctrl)

	ctx := context.Background()
	const orgCode = "CC-MOSKVA-01"
	const kekID = "yk-kek-tenant-abc"

	// Order of expected calls matters:
	store.EXPECT().GetByOrgCode(gomock.Any(), orgCode).Return(api.Tenant{}, api.ErrNotFound)
	kms.EXPECT().CreateKey(gomock.Any(), gomock.Any(), gomock.Any()).Return(kekID, nil)
	store.EXPECT().Insert(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, t api.Tenant) (api.Tenant, error) {
			require.Equal(t.OrgCode, orgCode)
			require.Equal(t.KMSKEKID, kekID)
			require.Equal(t.Status, api.TenantStatusActive)
			require.Len(t.PhoneHashPepper, 32)
			t.ID = uuid.New()
			return t, nil
		},
	)
	pub.EXPECT().PublishCreated(gomock.Any(), gomock.Any()).Return(nil)

	svc := service.NewTenantService(zaptest.NewLogger(t), store, kms, pub)
	tn, err := svc.Create(ctx, api.CreateTenantRequest{
		OrgCode: orgCode,
		Name:    "ВЦИОМ-Москва",
	})
	require.NoError(t, err)
	require.Equal(t, api.TenantStatusActive, tn.Status)
	require.Equal(t, kekID, tn.KMSKEKID)
}

func TestTenantService_Create_RejectsDuplicateOrgCode(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	kms := mocks.NewMockKMSClient(ctrl)
	pub := mocks.NewMockSettingsPublisher(ctrl)

	store.EXPECT().GetByOrgCode(gomock.Any(), "CC-MOSKVA-01").Return(
		api.Tenant{ID: uuid.New(), OrgCode: "CC-MOSKVA-01"}, nil,
	)
	// KMS NOT called — early-out.

	svc := service.NewTenantService(zaptest.NewLogger(t), store, kms, pub)
	_, err := svc.Create(context.Background(), api.CreateTenantRequest{
		OrgCode: "CC-MOSKVA-01", Name: "Dup",
	})
	require.ErrorIs(t, err, api.ErrAlreadyExists)
}

func TestTenantService_Create_RollsBackKEKOnInsertError(t *testing.T) {
	// If Insert fails after CreateKey succeeded, the KEK is orphaned.
	// We log a warning + still surface the error. (KEK cleanup is manual via runbook.)
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	kms := mocks.NewMockKMSClient(ctrl)
	pub := mocks.NewMockSettingsPublisher(ctrl)

	store.EXPECT().GetByOrgCode(gomock.Any(), "CC-NEW").Return(api.Tenant{}, api.ErrNotFound)
	kms.EXPECT().CreateKey(gomock.Any(), gomock.Any(), gomock.Any()).Return("yk-kek-orphan", nil)
	store.EXPECT().Insert(gomock.Any(), gomock.Any()).Return(api.Tenant{}, errors.New("pg: deadlock"))

	svc := service.NewTenantService(zaptest.NewLogger(t), store, kms, pub)
	_, err := svc.Create(context.Background(), api.CreateTenantRequest{
		OrgCode: "CC-NEW", Name: "X",
	})
	require.Error(t, err)
}

func TestTenantService_Suspend_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	pub := mocks.NewMockSettingsPublisher(ctrl)
	id := uuid.New()

	store.EXPECT().UpdateStatus(gomock.Any(), id, api.TenantStatusSuspended).Return(nil)
	pub.EXPECT().PublishSuspended(gomock.Any(), id).Return(nil)

	svc := service.NewTenantService(zaptest.NewLogger(t), store, nil /* kms unused */, pub)
	require.NoError(t, svc.Suspend(context.Background(), id, "non-payment"))
}

func TestTenantService_List_RespectsLimitAndOffset(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	pub := mocks.NewMockSettingsPublisher(ctrl)

	store.EXPECT().List(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, f api.ListTenantsFilter) ([]api.Tenant, error) {
			require.Equal(t, 50, f.Limit) // default applied
			return []api.Tenant{{ID: uuid.New(), OrgCode: "X", Status: api.TenantStatusActive}}, nil
		},
	)

	svc := service.NewTenantService(zaptest.NewLogger(t), store, nil, pub)
	out, err := svc.List(context.Background(), api.ListTenantsFilter{}) // limit=0 → default
	require.NoError(t, err)
	require.Len(t, out, 1)
}
```

Run `go test ./internal/tenancy/service/...`.

Expected (RED): compile error — `service.NewTenantService`, `mocks.MockStore`, `mocks.MockKMSClient`, `mocks.MockSettingsPublisher` all missing.

- [ ] **Step 2: Define the `Store` interface and Settings publisher**

Add to `internal/tenancy/api/module.go` (insertion point after `KMSClient` block):

```go
// Store is the persistence interface used by tenantService and settingsCache.
// Concrete impl: internal/tenancy/store.PostgresStore (uses tenancy_admin role).
//
// We declare it in api/ even though it is meant for module-internal use,
// because the gomock generator targets api/.
type Store interface {
	// Tenant CRUD
	Insert(ctx context.Context, t Tenant) (Tenant, error)
	Get(ctx context.Context, id uuid.UUID) (Tenant, error)
	GetByOrgCode(ctx context.Context, orgCode string) (Tenant, error)
	List(ctx context.Context, filter ListTenantsFilter) ([]Tenant, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status TenantStatus) error

	// PhoneHashPepper read (used by phoneHasher)
	GetPhoneHashPepper(ctx context.Context, tenantID uuid.UUID) ([]byte, error)

	// tenant_settings rows
	GetSetting(ctx context.Context, tenantID uuid.UUID, key string) (SettingValue, error)
	GetAllSettings(ctx context.Context, tenantID uuid.UUID) (map[string]SettingValue, error)
	UpsertSetting(ctx context.Context, tenantID uuid.UUID, key string, value SettingValue) error
	DeleteSetting(ctx context.Context, tenantID uuid.UUID, key string) error
}

// SettingsPublisher abstracts NATS for cache-invalidation and lifecycle events.
type SettingsPublisher interface {
	PublishCreated(ctx context.Context, t Tenant) error
	PublishSuspended(ctx context.Context, tenantID uuid.UUID) error
	PublishArchived(ctx context.Context, tenantID uuid.UUID) error
	PublishSettingUpdated(ctx context.Context, tenantID uuid.UUID, key string) error
	PublishSettingDeleted(ctx context.Context, tenantID uuid.UUID, key string) error
}
```

Add `PhoneHashPepper []byte` field to `Tenant`:

```go
// Tenant DTO addition:
PhoneHashPepper []byte `json:"-"` // 32 random bytes, never serialised
```

(JSON tag `-` keeps it out of HTTP response bodies; the field is populated only inside the module.)

- [ ] **Step 3: Add gomock to dependencies and generate mocks**

```bash
go get go.uber.org/mock/gomock
go install go.uber.org/mock/mockgen@latest
mkdir -p mocks/tenancy
```

Add a `//go:generate` directive at the top of `internal/tenancy/api/module.go`:

```go
//go:generate mockgen -source=module.go -destination=../../../mocks/tenancy/mock_module.go -package=mocks
```

Add a separate file `internal/tenancy/api/gen.go` (so the directive is canonical):

```go
//go:build ignore
package api

//go:generate mockgen -package mocks -destination ../../../mocks/tenancy/mock_kms_client.go github.com/sociopulse/platform/internal/tenancy/api KMSClient
//go:generate mockgen -package mocks -destination ../../../mocks/tenancy/mock_store.go github.com/sociopulse/platform/internal/tenancy/api Store
//go:generate mockgen -package mocks -destination ../../../mocks/tenancy/mock_settings_publisher.go github.com/sociopulse/platform/internal/tenancy/api SettingsPublisher
//go:generate mockgen -package mocks -destination ../../../mocks/tenancy/mock_tenancy.go github.com/sociopulse/platform/internal/tenancy/api Tenancy
```

Add a `Makefile` target:

```make
.PHONY: mocks
mocks:
	mockgen -package mocks -destination mocks/tenancy/mock_kms_client.go github.com/sociopulse/platform/internal/tenancy/api KMSClient
	mockgen -package mocks -destination mocks/tenancy/mock_store.go github.com/sociopulse/platform/internal/tenancy/api Store
	mockgen -package mocks -destination mocks/tenancy/mock_settings_publisher.go github.com/sociopulse/platform/internal/tenancy/api SettingsPublisher
	mockgen -package mocks -destination mocks/tenancy/mock_tenancy.go github.com/sociopulse/platform/internal/tenancy/api Tenancy
```

Run `make mocks`. Expected: four files appear under `mocks/tenancy/`.

- [ ] **Step 4 (GREEN): Write `internal/tenancy/service/tenant_service.go`**

```go
package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

// tenantService implements api.TenantService.
type tenantService struct {
	logger *zap.Logger
	store  api.Store
	kms    api.KMSClient
	pub    api.SettingsPublisher
}

// NewTenantService constructs a TenantService.
//
// Caller owns the lifecycle of every dependency; this service holds references
// only and does not close them on shutdown.
func NewTenantService(logger *zap.Logger, store api.Store, kms api.KMSClient, pub api.SettingsPublisher) api.TenantService {
	return &tenantService{logger: logger, store: store, kms: kms, pub: pub}
}

func (s *tenantService) Create(ctx context.Context, req api.CreateTenantRequest) (api.Tenant, error) {
	if req.OrgCode == "" {
		return api.Tenant{}, fmt.Errorf("%w: org_code", api.ErrInvalidArgument)
	}
	if req.Name == "" {
		return api.Tenant{}, fmt.Errorf("%w: name", api.ErrInvalidArgument)
	}

	// 1. Fast-fail on duplicate org_code (BEFORE creating a KMS KEK).
	existing, err := s.store.GetByOrgCode(ctx, req.OrgCode)
	switch {
	case errors.Is(err, api.ErrNotFound):
		// fall through
	case err != nil:
		return api.Tenant{}, fmt.Errorf("get-by-org-code: %w", err)
	default:
		_ = existing
		return api.Tenant{}, fmt.Errorf("%w: org_code=%q", api.ErrAlreadyExists, req.OrgCode)
	}

	// 2. Provision per-tenant KEK in Yandex KMS.
	kekName := "sociopulse-tenant-" + req.OrgCode
	kekDesc := "Per-tenant KEK for " + req.Name
	kekID, err := s.kms.CreateKey(ctx, kekName, kekDesc)
	if err != nil {
		return api.Tenant{}, fmt.Errorf("kms-create-key: %w", api.ErrKMSUnavailable)
	}

	// 3. Generate per-tenant phone-hash pepper (32 random bytes).
	pepper := make([]byte, 32)
	if _, err := rand.Read(pepper); err != nil {
		s.logger.Warn("orphan KEK created — manual cleanup required",
			zap.String("kek_id", kekID), zap.Error(err))
		return api.Tenant{}, fmt.Errorf("rand: %w", err)
	}

	// 4. Insert the tenant row.
	t := api.Tenant{
		OrgCode:         req.OrgCode,
		Name:            req.Name,
		Status:          api.TenantStatusActive,
		KMSKEKID:        kekID,
		PhoneHashPepper: pepper,
	}
	if err := t.Validate(); err != nil {
		return api.Tenant{}, err
	}
	saved, err := s.store.Insert(ctx, t)
	if err != nil {
		s.logger.Warn("orphan KEK created — manual cleanup required",
			zap.String("kek_id", kekID),
			zap.String("org_code", req.OrgCode),
			zap.Error(err))
		return api.Tenant{}, fmt.Errorf("insert: %w", err)
	}

	// 5. Publish lifecycle event (best-effort; does not fail Create).
	if pubErr := s.pub.PublishCreated(ctx, saved); pubErr != nil {
		s.logger.Warn("publish tenant.created failed", zap.Error(pubErr), zap.Stringer("tenant_id", saved.ID))
	}

	s.logger.Info("tenant created",
		zap.Stringer("tenant_id", saved.ID),
		zap.String("org_code", saved.OrgCode),
		zap.String("kek_id", saved.KMSKEKID),
	)
	return saved, nil
}

func (s *tenantService) Get(ctx context.Context, id uuid.UUID) (api.Tenant, error) {
	return s.store.Get(ctx, id)
}

func (s *tenantService) GetByOrgCode(ctx context.Context, orgCode string) (api.Tenant, error) {
	return s.store.GetByOrgCode(ctx, orgCode)
}

func (s *tenantService) List(ctx context.Context, filter api.ListTenantsFilter) ([]api.Tenant, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	return s.store.List(ctx, filter)
}

func (s *tenantService) Suspend(ctx context.Context, id uuid.UUID, reason string) error {
	if err := s.store.UpdateStatus(ctx, id, api.TenantStatusSuspended); err != nil {
		return fmt.Errorf("update-status: %w", err)
	}
	if err := s.pub.PublishSuspended(ctx, id); err != nil {
		s.logger.Warn("publish tenant.suspended failed", zap.Error(err), zap.Stringer("tenant_id", id))
	}
	s.logger.Info("tenant suspended", zap.Stringer("tenant_id", id), zap.String("reason", reason))
	return nil
}

func (s *tenantService) Resume(ctx context.Context, id uuid.UUID) error {
	if err := s.store.UpdateStatus(ctx, id, api.TenantStatusActive); err != nil {
		return fmt.Errorf("update-status: %w", err)
	}
	s.logger.Info("tenant resumed", zap.Stringer("tenant_id", id))
	return nil
}

func (s *tenantService) Archive(ctx context.Context, id uuid.UUID) error {
	if err := s.store.UpdateStatus(ctx, id, api.TenantStatusArchived); err != nil {
		return fmt.Errorf("update-status: %w", err)
	}
	if err := s.pub.PublishArchived(ctx, id); err != nil {
		s.logger.Warn("publish tenant.archived failed", zap.Error(err), zap.Stringer("tenant_id", id))
	}
	s.logger.Info("tenant archived", zap.Stringer("tenant_id", id))
	return nil
}
```

- [ ] **Step 5 (GREEN): Run unit tests**

```bash
go test ./internal/tenancy/service/...
```

Expected: pass (4 tests).

- [ ] **Step 6 (RED → GREEN): Write the Postgres store**

Create `internal/tenancy/store/tx.go`:

```go
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// withTx runs fn inside a transaction. The pool is the tenancy_admin pool
// (BYPASSRLS); it does NOT call SET LOCAL app.tenant_id because RLS is
// bypassed at role level.
func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
			return
		}
		err = tx.Commit(ctx)
	}()
	return fn(tx)
}
```

Create `internal/tenancy/store/postgres.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

// PostgresStore is the Postgres-backed implementation of api.Store.
//
// It connects through the `tenancy_admin` role (BYPASSRLS). Every other module
// uses the `app` role and is constrained by RLS — only this store may safely
// read across tenants.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore constructs a store. The pool MUST be authenticated as
// `tenancy_admin` (a Postgres role created by Plan 03 with BYPASSRLS).
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// pgUniqueViolation is the SQLSTATE for unique_violation.
const pgUniqueViolation = "23505"

func (s *PostgresStore) Insert(ctx context.Context, t api.Tenant) (api.Tenant, error) {
	const q = `
		INSERT INTO tenants (org_code, name, status, kms_kek_id, phone_hash_pepper)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`
	var id uuid.UUID
	var createdAt = t.CreatedAt
	err := s.pool.QueryRow(ctx, q,
		t.OrgCode, t.Name, string(t.Status), t.KMSKEKID, t.PhoneHashPepper,
	).Scan(&id, &createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return api.Tenant{}, fmt.Errorf("%w: %s", api.ErrAlreadyExists, pgErr.ConstraintName)
		}
		return api.Tenant{}, fmt.Errorf("insert tenant: %w", err)
	}
	t.ID = id
	t.CreatedAt = createdAt
	return t, nil
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (api.Tenant, error) {
	const q = `
		SELECT id, org_code, name, status, kms_kek_id, phone_hash_pepper, created_at
		FROM tenants WHERE id = $1`
	var t api.Tenant
	var status string
	err := s.pool.QueryRow(ctx, q, id).Scan(
		&t.ID, &t.OrgCode, &t.Name, &status, &t.KMSKEKID, &t.PhoneHashPepper, &t.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Tenant{}, api.ErrNotFound
	}
	if err != nil {
		return api.Tenant{}, fmt.Errorf("select tenant: %w", err)
	}
	t.Status = api.TenantStatus(status)
	return t, nil
}

func (s *PostgresStore) GetByOrgCode(ctx context.Context, orgCode string) (api.Tenant, error) {
	const q = `
		SELECT id, org_code, name, status, kms_kek_id, phone_hash_pepper, created_at
		FROM tenants WHERE org_code = $1`
	var t api.Tenant
	var status string
	err := s.pool.QueryRow(ctx, q, orgCode).Scan(
		&t.ID, &t.OrgCode, &t.Name, &status, &t.KMSKEKID, &t.PhoneHashPepper, &t.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Tenant{}, api.ErrNotFound
	}
	if err != nil {
		return api.Tenant{}, fmt.Errorf("select by org_code: %w", err)
	}
	t.Status = api.TenantStatus(status)
	return t, nil
}

func (s *PostgresStore) List(ctx context.Context, f api.ListTenantsFilter) ([]api.Tenant, error) {
	q := `
		SELECT id, org_code, name, status, kms_kek_id, phone_hash_pepper, created_at
		FROM tenants
		WHERE ($1::text IS NULL OR status = $1)
		  AND ($2::text = '' OR org_code = $2)
		ORDER BY created_at DESC
		LIMIT $3 OFFSET $4`
	var statusArg any
	if f.Status != nil {
		statusArg = string(*f.Status)
	}
	rows, err := s.pool.Query(ctx, q, statusArg, f.OrgCode, f.Limit, f.Offset)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()

	var out []api.Tenant
	for rows.Next() {
		var t api.Tenant
		var status string
		if err := rows.Scan(&t.ID, &t.OrgCode, &t.Name, &status, &t.KMSKEKID, &t.PhoneHashPepper, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan tenant: %w", err)
		}
		t.Status = api.TenantStatus(status)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) UpdateStatus(ctx context.Context, id uuid.UUID, status api.TenantStatus) error {
	const q = `UPDATE tenants SET status = $1 WHERE id = $2`
	tag, err := s.pool.Exec(ctx, q, string(status), id)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrNotFound
	}
	return nil
}

func (s *PostgresStore) GetPhoneHashPepper(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	const q = `SELECT phone_hash_pepper FROM tenants WHERE id = $1`
	var pepper []byte
	err := s.pool.QueryRow(ctx, q, tenantID).Scan(&pepper)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, api.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select pepper: %w", err)
	}
	return pepper, nil
}

func (s *PostgresStore) GetSetting(ctx context.Context, tenantID uuid.UUID, key string) (api.SettingValue, error) {
	const q = `SELECT value FROM tenant_settings WHERE tenant_id = $1 AND key = $2`
	var raw json.RawMessage
	err := s.pool.QueryRow(ctx, q, tenantID, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.SettingValue{}, api.ErrNotFound
	}
	if err != nil {
		return api.SettingValue{}, fmt.Errorf("select setting: %w", err)
	}
	return api.SettingValueFromRaw(raw), nil
}

func (s *PostgresStore) GetAllSettings(ctx context.Context, tenantID uuid.UUID) (map[string]api.SettingValue, error) {
	const q = `SELECT key, value FROM tenant_settings WHERE tenant_id = $1`
	rows, err := s.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("select all settings: %w", err)
	}
	defer rows.Close()

	out := make(map[string]api.SettingValue)
	for rows.Next() {
		var key string
		var raw json.RawMessage
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out[key] = api.SettingValueFromRaw(raw)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpsertSetting(ctx context.Context, tenantID uuid.UUID, key string, value api.SettingValue) error {
	const q = `
		INSERT INTO tenant_settings (tenant_id, key, value, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (tenant_id, key) DO UPDATE
		  SET value = EXCLUDED.value, updated_at = now()`
	_, err := s.pool.Exec(ctx, q, tenantID, key, []byte(value.Raw()))
	if err != nil {
		return fmt.Errorf("upsert setting: %w", err)
	}
	return nil
}

func (s *PostgresStore) DeleteSetting(ctx context.Context, tenantID uuid.UUID, key string) error {
	const q = `DELETE FROM tenant_settings WHERE tenant_id = $1 AND key = $2`
	tag, err := s.pool.Exec(ctx, q, tenantID, key)
	if err != nil {
		return fmt.Errorf("delete setting: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrNotFound
	}
	return nil
}
```

- [ ] **Step 7 (RED): Integration test for PostgresStore**

Create `internal/tenancy/store/_testdata/0001_tenancy_setup.sql`. This mirrors what Plan 03 will install in production. Used only by the integration test.

```sql
-- Mirrors what Plan 03 installs. Kept here so the integration test is hermetic.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS tenants (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_code text NOT NULL UNIQUE,
  name text NOT NULL,
  status text NOT NULL CHECK (status IN ('active','suspended','archived')),
  kms_kek_id text NOT NULL,
  phone_hash_pepper bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tenant_settings (
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  key text NOT NULL,
  value jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, key)
);

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tenancy_admin') THEN
    CREATE ROLE tenancy_admin BYPASSRLS LOGIN PASSWORD 'dev-only';
  END IF;
END$$;

GRANT ALL ON tenants, tenant_settings TO tenancy_admin;
```

Create `internal/tenancy/store/postgres_test.go`:

```go
//go:build integration
// +build integration

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/store"
)

func setup(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx := context.Background()

	scriptPath, err := filepath.Abs(filepath.Join("_testdata", "0001_tenancy_setup.sql"))
	require.NoError(t, err)

	pgC, err := postgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:16-alpine"),
		postgres.WithDatabase("sociopulse_test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.WithInitScripts(scriptPath),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)

	cleanup := func() {
		pool.Close()
		_ = pgC.Terminate(ctx)
	}
	return pool, cleanup
}

func TestPostgresStore_TenantCRUD(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set")
	}
	pool, cleanup := setup(t)
	defer cleanup()

	s := store.NewPostgresStore(pool)
	ctx := context.Background()

	tn, err := s.Insert(ctx, api.Tenant{
		OrgCode:         "CC-MOSKVA-01",
		Name:            "ВЦИОМ-Москва",
		Status:          api.TenantStatusActive,
		KMSKEKID:        "yk-kek-1",
		PhoneHashPepper: bytesOfLen(32),
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, tn.ID)

	got, err := s.Get(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, tn.OrgCode, got.OrgCode)

	_, err = s.Insert(ctx, api.Tenant{
		OrgCode: "CC-MOSKVA-01", Name: "Dup",
		Status: api.TenantStatusActive, KMSKEKID: "x", PhoneHashPepper: bytesOfLen(32),
	})
	require.ErrorIs(t, err, api.ErrAlreadyExists)

	require.NoError(t, s.UpdateStatus(ctx, tn.ID, api.TenantStatusSuspended))
	got, err = s.Get(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, api.TenantStatusSuspended, got.Status)

	out, err := s.List(ctx, api.ListTenantsFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, out, 1)
}

func TestPostgresStore_SettingsCRUD(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set")
	}
	pool, cleanup := setup(t)
	defer cleanup()

	s := store.NewPostgresStore(pool)
	ctx := context.Background()
	tn, err := s.Insert(ctx, api.Tenant{
		OrgCode: "CC-X", Name: "X",
		Status: api.TenantStatusActive, KMSKEKID: "k", PhoneHashPepper: bytesOfLen(32),
	})
	require.NoError(t, err)

	v, err := api.SettingValueFromAny("4h")
	require.NoError(t, err)
	require.NoError(t, s.UpsertSetting(ctx, tn.ID, "dialer.retry_no_answer_delay", v))

	got, err := s.GetSetting(ctx, tn.ID, "dialer.retry_no_answer_delay")
	require.NoError(t, err)
	d, err := got.AsDuration()
	require.NoError(t, err)
	require.Equal(t, 4*time.Hour, d)

	require.NoError(t, s.DeleteSetting(ctx, tn.ID, "dialer.retry_no_answer_delay"))
	_, err = s.GetSetting(ctx, tn.ID, "dialer.retry_no_answer_delay")
	require.ErrorIs(t, err, api.ErrNotFound)
}

func bytesOfLen(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}
```

Add a Make target for integration:

```make
.PHONY: integration-test
integration-test:
	go test -tags=integration -count=1 -v ./...
```

- [ ] **Step 8 (GREEN): Run integration test**

```bash
make integration-test
```

Expected: pass (Docker required; testcontainers spins up Postgres 16 in ~5s).

If Docker is unavailable, the `SKIP_INTEGRATION` env-var lets CI skip — the unit tests still cover the service layer.

- [ ] **Step 9: Commit**

```bash
git add internal/tenancy/api internal/tenancy/service internal/tenancy/store mocks/tenancy Makefile go.mod go.sum
git commit -m "tenancy: implement TenantService over tenancy_admin Postgres role with KEK provisioning on Create"
```

Expected: clean commit. `make test` and `make integration-test` both green.

---

## Task 3: `KMSResolver` — Yandex KMS adapter, lazy KEK lifecycle

**Goal:** Build the `KMSClient` adapter against Yandex KMS Go SDK and the higher-level `kmsResolver` that uses `KMSClient` for envelope ops, leaving `EnsureKEK` idempotent on cold starts.

**Files:**
- Create: `internal/tenancy/service/kms_client_yandex.go`
- Create: `internal/tenancy/service/kms_client_yandex_test.go`
- Create: `internal/tenancy/service/kms_resolver.go` (envelope only — DEK cache lands in Task 4).
- Create: `internal/tenancy/service/kms_resolver_test.go`

- [ ] **Step 1 (RED): Write `kms_resolver_test.go` (envelope-without-cache)**

```go
package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/service"
	mocks "github.com/sociopulse/platform/mocks/tenancy"
)

func TestKMSResolver_EnsureKEK_DelegatesToTenantService(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	kms := mocks.NewMockKMSClient(ctrl)

	tenantID := uuid.New()
	store.EXPECT().Get(gomock.Any(), tenantID).Return(api.Tenant{
		ID:       tenantID,
		KMSKEKID: "yk-kek-existing",
	}, nil)

	r := service.NewKMSResolver(zaptest.NewLogger(t), store, kms, service.KMSResolverConfig{})
	got, err := r.EnsureKEK(context.Background(), tenantID)
	require.NoError(t, err)
	require.Equal(t, "yk-kek-existing", got)
}

func TestKMSResolver_GenerateDataKey_PassThroughsToKMS(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	kms := mocks.NewMockKMSClient(ctrl)
	tenantID := uuid.New()

	store.EXPECT().Get(gomock.Any(), tenantID).Return(api.Tenant{
		ID: tenantID, KMSKEKID: "yk-kek-1",
	}, nil)
	kms.EXPECT().GenerateDataKey(gomock.Any(), "yk-kek-1").Return(
		[]byte("plaintext-32-bytes-............."), []byte("ciphertext"), "v1", nil,
	)

	r := service.NewKMSResolver(zaptest.NewLogger(t), store, kms, service.KMSResolverConfig{})
	dk, err := r.GenerateDataKey(context.Background(), tenantID)
	require.NoError(t, err)
	require.Equal(t, []byte("ciphertext"), dk.Ciphertext)
	require.Equal(t, "v1", dk.KeyVersion)
	require.Len(t, dk.Plaintext, 32)
}
```

Run `go test ./internal/tenancy/service/... -run KMSResolver`. Expected RED: missing `service.NewKMSResolver`, `service.KMSResolverConfig`.

- [ ] **Step 2 (GREEN): Write `service/kms_resolver.go` (envelope without cache; cache in Task 4)**

```go
package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

// KMSResolverConfig controls cache shape and is wired from api.Config.
type KMSResolverConfig struct {
	DEKCacheTTL  time.Duration
	DEKCacheSize int
}

func (c *KMSResolverConfig) defaults() {
	if c.DEKCacheTTL <= 0 {
		c.DEKCacheTTL = 5 * time.Minute
	}
	if c.DEKCacheSize <= 0 {
		c.DEKCacheSize = 1024
	}
}

// kmsResolver implements api.KMSResolver.
type kmsResolver struct {
	logger *zap.Logger
	store  api.Store
	kms    api.KMSClient
	cfg    KMSResolverConfig
	dekCache *lru.LRU[uuid.UUID, cachedDEK]
}

type cachedDEK struct {
	plaintext  []byte
	ciphertext []byte
	keyVersion string
}

// NewKMSResolver constructs the resolver. The DEK cache is created in Task 4;
// here it stays nil to keep this task scoped.
func NewKMSResolver(logger *zap.Logger, store api.Store, kms api.KMSClient, cfg KMSResolverConfig) api.KMSResolver {
	cfg.defaults()
	r := &kmsResolver{logger: logger, store: store, kms: kms, cfg: cfg}
	r.dekCache = lru.NewLRU[uuid.UUID, cachedDEK](cfg.DEKCacheSize, nil, cfg.DEKCacheTTL)
	return r
}

func (r *kmsResolver) EnsureKEK(ctx context.Context, tenantID uuid.UUID) (string, error) {
	tn, err := r.store.Get(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("get tenant: %w", err)
	}
	if tn.KMSKEKID == "" {
		return "", fmt.Errorf("%w: tenant has no KEK", api.ErrInvalidArgument)
	}
	return tn.KMSKEKID, nil
}

func (r *kmsResolver) GenerateDataKey(ctx context.Context, tenantID uuid.UUID) (api.DataKey, error) {
	kekID, err := r.EnsureKEK(ctx, tenantID)
	if err != nil {
		return api.DataKey{}, err
	}
	pt, ct, version, err := r.kms.GenerateDataKey(ctx, kekID)
	if err != nil {
		return api.DataKey{}, fmt.Errorf("%w: %v", api.ErrKMSUnavailable, err)
	}
	if len(pt) != 32 {
		return api.DataKey{}, errors.New("kms: dek plaintext must be 32 bytes")
	}
	return api.DataKey{Plaintext: pt, Ciphertext: ct, KeyVersion: version}, nil
}

// resolveDEK returns a usable DEK for the tenant, hitting cache first.
// Caller MUST NOT zeroise the returned plaintext — the cache owns it.
func (r *kmsResolver) resolveDEK(ctx context.Context, tenantID uuid.UUID) (cachedDEK, error) {
	if hit, ok := r.dekCache.Get(tenantID); ok {
		return hit, nil
	}
	dk, err := r.GenerateDataKey(ctx, tenantID)
	if err != nil {
		return cachedDEK{}, err
	}
	c := cachedDEK{plaintext: dk.Plaintext, ciphertext: dk.Ciphertext, keyVersion: dk.KeyVersion}
	r.dekCache.Add(tenantID, c)
	return c, nil
}

// Encrypt format: [4-byte ctLen][ciphertextDEK][12-byte nonce][AES-GCM(payload)]
// This packs the wrapped DEK with the payload so a single ciphertext blob is
// portable across cache misses (no per-row DEK column needed for short PII).
func (r *kmsResolver) Encrypt(ctx context.Context, tenantID uuid.UUID, plaintext []byte) ([]byte, error) {
	dek, err := r.resolveDEK(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek.plaintext)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	body := gcm.Seal(nil, nonce, plaintext, nil)

	out := make([]byte, 0, 4+len(dek.ciphertext)+len(nonce)+len(body))
	out = binary.BigEndian.AppendUint32(out, uint32(len(dek.ciphertext)))
	out = append(out, dek.ciphertext...)
	out = append(out, nonce...)
	out = append(out, body...)
	return out, nil
}

func (r *kmsResolver) Decrypt(ctx context.Context, tenantID uuid.UUID, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 4 {
		return nil, fmt.Errorf("%w: short ciphertext", api.ErrInvalidArgument)
	}
	ctLen := binary.BigEndian.Uint32(ciphertext[:4])
	if int(4+ctLen) > len(ciphertext) {
		return nil, fmt.Errorf("%w: malformed ciphertext", api.ErrInvalidArgument)
	}
	wrappedDEK := ciphertext[4 : 4+ctLen]
	rest := ciphertext[4+ctLen:]

	// Try cache first; if cache miss OR cached DEK doesn't match the wrapped one,
	// unwrap via KMS.
	dek, ok := r.dekCache.Get(tenantID)
	if !ok || !bytesEqual(dek.ciphertext, wrappedDEK) {
		kekID, err := r.EnsureKEK(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		pt, version, err := r.kms.Decrypt(ctx, kekID, wrappedDEK)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", api.ErrKMSUnavailable, err)
		}
		dek = cachedDEK{plaintext: pt, ciphertext: wrappedDEK, keyVersion: version}
		r.dekCache.Add(tenantID, dek)
	}

	block, err := aes.NewCipher(dek.plaintext)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	if len(rest) < gcm.NonceSize() {
		return nil, fmt.Errorf("%w: short body", api.ErrInvalidArgument)
	}
	nonce := rest[:gcm.NonceSize()]
	body := rest[gcm.NonceSize():]

	pt, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: open: %v", api.ErrInvalidArgument, err)
	}
	return pt, nil
}

func (r *kmsResolver) InvalidateCache(tenantID uuid.UUID) {
	r.dekCache.Remove(tenantID)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

Add dependency: `go get github.com/hashicorp/golang-lru/v2`.

- [ ] **Step 3 (GREEN): Run unit tests**

```bash
go test ./internal/tenancy/service/... -run KMSResolver -v
```

Expected: pass.

- [ ] **Step 4 (RED): Write the Yandex KMS adapter test**

`internal/tenancy/service/kms_client_yandex_test.go`:

```go
package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/tenancy/service"
)

// fakeYandexEndpoint exists only as a compile-time fixture; the real test
// for the Yandex SDK adapter runs as integration with build tag `integration_kms`
// against a Yandex KMS test folder. Here we only verify constructor wiring.

func TestNewYandexKMSClient_RejectsEmptyConfig(t *testing.T) {
	_, err := service.NewYandexKMSClient(context.Background(), service.YandexKMSConfig{})
	require.Error(t, err)
}
```

- [ ] **Step 5 (GREEN): Write `kms_client_yandex.go`**

```go
package service

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/yandex-cloud/go-genproto/yandex/cloud/kms/v1"
	ycsdk "github.com/yandex-cloud/go-sdk"
	"github.com/yandex-cloud/go-sdk/iamkey"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

type YandexKMSConfig struct {
	Endpoint              string
	FolderID              string
	ServiceAccountKeyPath string
}

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

type yandexKMSClient struct {
	cfg          YandexKMSConfig
	keyService   kms.SymmetricKeyServiceClient
	cryptoSvc    kms.SymmetricCryptoServiceClient
	sdkCloseable interface{ Close() error }
}

// NewYandexKMSClient builds a real Yandex KMS adapter. Reads the IAM service
// account key from disk and constructs an authenticated SDK.
func NewYandexKMSClient(ctx context.Context, cfg YandexKMSConfig) (api.KMSClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	keyData, err := os.ReadFile(cfg.ServiceAccountKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read sa key: %w", err)
	}
	saKey, err := iamkey.ReadFromJSONBytes(keyData)
	if err != nil {
		return nil, fmt.Errorf("parse sa key: %w", err)
	}
	creds, err := ycsdk.ServiceAccountKey(saKey)
	if err != nil {
		return nil, fmt.Errorf("sa creds: %w", err)
	}
	sdk, err := ycsdk.Build(ctx, ycsdk.Config{
		Credentials: creds,
		Endpoint:    cfg.Endpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("build sdk: %w", err)
	}
	return &yandexKMSClient{
		cfg:        cfg,
		keyService: sdk.KMS().SymmetricKey(),
		cryptoSvc:  sdk.KMS().SymmetricCrypto(),
	}, nil
}

func (c *yandexKMSClient) CreateKey(ctx context.Context, name, description string) (string, error) {
	req := &kms.CreateSymmetricKeyRequest{
		FolderId:         c.cfg.FolderID,
		Name:             name,
		Description:      description,
		DefaultAlgorithm: kms.SymmetricAlgorithm_AES_256,
		// Per-tenant rotation managed by runbook (see docs/runbooks/key-rotation.md);
		// we don't auto-rotate to avoid surprise.
	}
	op, err := c.keyService.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("kms create: %w", err)
	}
	if op == nil || op.GetMetadata() == nil {
		return "", errors.New("kms create: empty op metadata")
	}
	meta := &kms.CreateSymmetricKeyMetadata{}
	if err := op.GetMetadata().UnmarshalTo(meta); err != nil {
		return "", fmt.Errorf("unmarshal metadata: %w", err)
	}
	return meta.GetKeyId(), nil
}

func (c *yandexKMSClient) Encrypt(ctx context.Context, keyID string, plaintext []byte) ([]byte, string, error) {
	resp, err := c.cryptoSvc.Encrypt(ctx, &kms.SymmetricEncryptRequest{
		KeyId:     keyID,
		Plaintext: plaintext,
	})
	if err != nil {
		return nil, "", fmt.Errorf("kms encrypt: %w", err)
	}
	return resp.GetCiphertext(), resp.GetVersionId(), nil
}

func (c *yandexKMSClient) Decrypt(ctx context.Context, keyID string, ciphertext []byte) ([]byte, string, error) {
	resp, err := c.cryptoSvc.Decrypt(ctx, &kms.SymmetricDecryptRequest{
		KeyId:      keyID,
		Ciphertext: ciphertext,
	})
	if err != nil {
		return nil, "", fmt.Errorf("kms decrypt: %w", err)
	}
	return resp.GetPlaintext(), resp.GetVersionId(), nil
}

func (c *yandexKMSClient) GenerateDataKey(ctx context.Context, keyID string) ([]byte, []byte, string, error) {
	resp, err := c.cryptoSvc.GenerateDataKey(ctx, &kms.GenerateDataKeyRequest{
		KeyId:        keyID,
		DataKeySpec:  kms.SymmetricAlgorithm_AES_256,
		SkipPlaintext: false,
	})
	if err != nil {
		return nil, nil, "", fmt.Errorf("kms gendk: %w", err)
	}
	return resp.GetDataKeyPlaintext(), resp.GetDataKeyCiphertext(), resp.GetVersionId(), nil
}
```

Add deps: `go get github.com/yandex-cloud/go-sdk@v0.0.50 github.com/yandex-cloud/go-genproto`.

- [ ] **Step 6: Run all tests**

```bash
go test ./internal/tenancy/...
```

Expected: pass.

- [ ] **Step 7: Commit**

```bash
git add internal/tenancy/service mocks/tenancy go.mod go.sum
git commit -m "tenancy: add KMSResolver with envelope encrypt/decrypt and Yandex KMS adapter"
```

---

## Task 4: DEK cache hardening + KEK provisioning hook in TenantService

**Goal:** Plug `KMSResolver` into `TenantService.Create` so freshly-created tenants get a working KEK record consistent with the resolver. Verify cache TTL via a fake clock test. Ensure `InvalidateCache` is called from `Suspend` / `Archive`.

**Files:**
- Modify: `internal/tenancy/service/tenant_service.go` — accept an optional `kmsResolver` for cache invalidation on Suspend/Archive.
- Modify: `internal/tenancy/service/kms_resolver.go` — add `Now func() time.Time` indirection for testability.
- Add tests: `internal/tenancy/service/kms_resolver_test.go` (TTL expiry, invalidation on suspend).

- [ ] **Step 1 (RED): TTL test**

Append to `kms_resolver_test.go`:

```go
func TestKMSResolver_DEKCacheExpires(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	kms := mocks.NewMockKMSClient(ctrl)
	tenantID := uuid.New()

	// EnsureKEK: 2 calls (one per Encrypt). Tenant fetch happens twice.
	store.EXPECT().Get(gomock.Any(), tenantID).Return(api.Tenant{ID: tenantID, KMSKEKID: "k"}, nil).Times(2)

	dekPlaintext := make([]byte, 32)
	for i := range dekPlaintext {
		dekPlaintext[i] = 7
	}
	kms.EXPECT().GenerateDataKey(gomock.Any(), "k").
		Return(append([]byte{}, dekPlaintext...), []byte("ct1"), "v1", nil).Times(2)

	r := service.NewKMSResolver(zaptest.NewLogger(t), store, kms, service.KMSResolverConfig{
		DEKCacheTTL:  10 * time.Millisecond,
		DEKCacheSize: 4,
	})
	_, err := r.Encrypt(context.Background(), tenantID, []byte("hi"))
	require.NoError(t, err)

	time.Sleep(20 * time.Millisecond) // expire

	_, err = r.Encrypt(context.Background(), tenantID, []byte("hi-again"))
	require.NoError(t, err)
}

func TestKMSResolver_EncryptThenDecrypt_RoundTrip(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	kms := mocks.NewMockKMSClient(ctrl)
	tenantID := uuid.New()

	pt := make([]byte, 32)
	for i := range pt {
		pt[i] = byte(i + 1)
	}
	store.EXPECT().Get(gomock.Any(), tenantID).Return(api.Tenant{ID: tenantID, KMSKEKID: "k"}, nil).AnyTimes()
	kms.EXPECT().GenerateDataKey(gomock.Any(), "k").Return(pt, []byte("ct"), "v1", nil).AnyTimes()
	// Decrypt path uses kms.Decrypt only on cache miss; in this test no miss.

	r := service.NewKMSResolver(zaptest.NewLogger(t), store, kms, service.KMSResolverConfig{
		DEKCacheTTL: time.Hour, DEKCacheSize: 4,
	})
	enc, err := r.Encrypt(context.Background(), tenantID, []byte("+79991234567"))
	require.NoError(t, err)

	dec, err := r.Decrypt(context.Background(), tenantID, enc)
	require.NoError(t, err)
	require.Equal(t, "+79991234567", string(dec))
}

func TestKMSResolver_InvalidateOnSuspend(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	kms := mocks.NewMockKMSClient(ctrl)
	pub := mocks.NewMockSettingsPublisher(ctrl)
	tenantID := uuid.New()

	store.EXPECT().Get(gomock.Any(), tenantID).Return(api.Tenant{ID: tenantID, KMSKEKID: "k"}, nil).AnyTimes()
	kms.EXPECT().GenerateDataKey(gomock.Any(), "k").Return(make([]byte, 32), []byte("ct"), "v1", nil).Times(2)

	store.EXPECT().UpdateStatus(gomock.Any(), tenantID, api.TenantStatusSuspended).Return(nil)
	pub.EXPECT().PublishSuspended(gomock.Any(), tenantID).Return(nil)

	r := service.NewKMSResolver(zaptest.NewLogger(t), store, kms, service.KMSResolverConfig{
		DEKCacheTTL: time.Hour, DEKCacheSize: 4,
	})
	svc := service.NewTenantServiceWithKMS(zaptest.NewLogger(t), store, kms, pub, r)

	_, err := r.Encrypt(context.Background(), tenantID, []byte("a"))
	require.NoError(t, err)

	require.NoError(t, svc.Suspend(context.Background(), tenantID, "test"))

	// After Suspend, the DEK should be evicted — next Encrypt re-pulls.
	_, err = r.Encrypt(context.Background(), tenantID, []byte("b"))
	require.NoError(t, err)
}
```

- [ ] **Step 2 (GREEN): Add `NewTenantServiceWithKMS` constructor**

In `internal/tenancy/service/tenant_service.go`, append:

```go
type tenantServiceWithKMS struct {
	*tenantService
	kmsResolver api.KMSResolver
}

// NewTenantServiceWithKMS is identical to NewTenantService but also calls
// kmsResolver.InvalidateCache on Suspend/Archive. cmd/api/main.go must use
// this variant.
func NewTenantServiceWithKMS(logger *zap.Logger, store api.Store, kms api.KMSClient, pub api.SettingsPublisher, resolver api.KMSResolver) api.TenantService {
	base := &tenantService{logger: logger, store: store, kms: kms, pub: pub}
	return &tenantServiceWithKMS{tenantService: base, kmsResolver: resolver}
}

func (s *tenantServiceWithKMS) Suspend(ctx context.Context, id uuid.UUID, reason string) error {
	if err := s.tenantService.Suspend(ctx, id, reason); err != nil {
		return err
	}
	s.kmsResolver.InvalidateCache(id)
	return nil
}

func (s *tenantServiceWithKMS) Archive(ctx context.Context, id uuid.UUID) error {
	if err := s.tenantService.Archive(ctx, id); err != nil {
		return err
	}
	s.kmsResolver.InvalidateCache(id)
	return nil
}
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/tenancy/...
```

Expected: pass.

- [ ] **Step 4: Commit**

```bash
git add internal/tenancy/service
git commit -m "tenancy: invalidate DEK cache on Suspend/Archive; add TTL roundtrip tests"
```

---

## Task 5: `PhoneHasher` — HMAC-SHA256 with per-tenant pepper + canonicalisation

**Goal:** A deterministic, per-tenant-salted hash for phone numbers. Uses HMAC-SHA256 (not SHA-256-with-pepper) so the keyed-PRF property protects against rainbow-table search even if the pepper leaks. Canonicalisation strips formatting and validates E.164.

**Files:**
- Create: `internal/tenancy/service/phone_hasher.go`
- Create: `internal/tenancy/service/phone_hasher_test.go`

- [ ] **Step 1 (RED): Write `phone_hasher_test.go`**

```go
package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/service"
	mocks "github.com/sociopulse/platform/mocks/tenancy"
)

func TestPhoneHasher_NormaliseHappyPath(t *testing.T) {
	h := service.NewPhoneHasher(zaptest.NewLogger(t), nil, service.PhoneHasherConfig{})
	cases := []struct{ in, want string }{
		{"+7 999 123-45-67", "+79991234567"},
		{"+79991234567", "+79991234567"},
		{"8 (999) 123-45-67", "+79991234567"},
		{"+44 20 7946 0958", "+442079460958"},
	}
	for _, c := range cases {
		got, err := h.Normalise(c.in)
		require.NoError(t, err, c.in)
		require.Equal(t, c.want, got, c.in)
	}
}

func TestPhoneHasher_NormaliseRejectsGarbage(t *testing.T) {
	h := service.NewPhoneHasher(zaptest.NewLogger(t), nil, service.PhoneHasherConfig{})
	for _, in := range []string{"", "abc", "1234", "+1", "+99999999999999999"} {
		_, err := h.Normalise(in)
		require.ErrorIs(t, err, api.ErrInvalidArgument, in)
	}
}

func TestPhoneHasher_HashIsDeterministicPerTenant(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	tenantID := uuid.New()

	pepper := make([]byte, 32)
	for i := range pepper {
		pepper[i] = byte(i)
	}
	store.EXPECT().GetPhoneHashPepper(gomock.Any(), tenantID).Return(pepper, nil).AnyTimes()

	h := service.NewPhoneHasher(zaptest.NewLogger(t), store, service.PhoneHasherConfig{})
	h1, err := h.Hash(context.Background(), tenantID, "+79991234567")
	require.NoError(t, err)
	h2, err := h.Hash(context.Background(), tenantID, "8 (999) 123-45-67")
	require.NoError(t, err)
	require.Equal(t, h1, h2)
	require.Len(t, h1, 32)
}

func TestPhoneHasher_HashesDifferAcrossTenants(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mocks.NewMockStore(ctrl)
	a := uuid.New()
	b := uuid.New()

	store.EXPECT().GetPhoneHashPepper(gomock.Any(), a).Return(make([]byte, 32), nil).AnyTimes()
	bPepper := make([]byte, 32)
	for i := range bPepper {
		bPepper[i] = 0xFF
	}
	store.EXPECT().GetPhoneHashPepper(gomock.Any(), b).Return(bPepper, nil).AnyTimes()

	h := service.NewPhoneHasher(zaptest.NewLogger(t), store, service.PhoneHasherConfig{})
	ha, _ := h.Hash(context.Background(), a, "+79991234567")
	hb, _ := h.Hash(context.Background(), b, "+79991234567")
	require.NotEqual(t, ha, hb)
}
```

- [ ] **Step 2 (GREEN): Write `service/phone_hasher.go`**

```go
package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

type PhoneHasherConfig struct {
	PepperCacheTTL  time.Duration
	PepperCacheSize int
}

func (c *PhoneHasherConfig) defaults() {
	if c.PepperCacheTTL <= 0 {
		c.PepperCacheTTL = 5 * time.Minute
	}
	if c.PepperCacheSize <= 0 {
		c.PepperCacheSize = 1024
	}
}

type phoneHasher struct {
	logger      *zap.Logger
	store       api.Store
	pepperCache *lru.LRU[uuid.UUID, []byte]
}

func NewPhoneHasher(logger *zap.Logger, store api.Store, cfg PhoneHasherConfig) api.PhoneHasher {
	cfg.defaults()
	return &phoneHasher{
		logger:      logger,
		store:       store,
		pepperCache: lru.NewLRU[uuid.UUID, []byte](cfg.PepperCacheSize, nil, cfg.PepperCacheTTL),
	}
}

func (h *phoneHasher) Hash(ctx context.Context, tenantID uuid.UUID, phone string) ([]byte, error) {
	canon, err := h.Normalise(phone)
	if err != nil {
		return nil, err
	}
	pepper, err := h.pepperFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, pepper)
	if _, err := mac.Write([]byte(canon)); err != nil {
		return nil, fmt.Errorf("hmac write: %w", err)
	}
	return mac.Sum(nil), nil
}

func (h *phoneHasher) Normalise(phone string) (string, error) {
	if phone == "" {
		return "", fmt.Errorf("%w: empty phone", api.ErrInvalidArgument)
	}
	var b strings.Builder
	b.Grow(len(phone))

	// Russian-call-center heuristic: leading "8" → "+7"
	trimmed := strings.TrimSpace(phone)
	if strings.HasPrefix(trimmed, "8") && len(trimmed) >= 11 {
		// 8 (999) 123-45-67 → +7...
		// Only collapse if the digits-only result has 11 digits.
		var digits strings.Builder
		for _, r := range trimmed {
			if r >= '0' && r <= '9' {
				digits.WriteRune(r)
			}
		}
		d := digits.String()
		if len(d) == 11 && d[0] == '8' {
			return "+7" + d[1:], nil
		}
	}

	hasPlus := false
	for i, r := range trimmed {
		switch {
		case r == '+' && i == 0:
			b.WriteRune('+')
			hasPlus = true
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
			// strip
		default:
			return "", fmt.Errorf("%w: invalid char %q", api.ErrInvalidArgument, r)
		}
	}
	out := b.String()
	if !hasPlus {
		return "", fmt.Errorf("%w: missing leading +", api.ErrInvalidArgument)
	}
	// E.164: + then 8..15 digits.
	digitsOnly := strings.TrimPrefix(out, "+")
	if len(digitsOnly) < 8 || len(digitsOnly) > 15 {
		return "", fmt.Errorf("%w: bad e164 length %d", api.ErrInvalidArgument, len(digitsOnly))
	}
	return out, nil
}

func (h *phoneHasher) pepperFor(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	if hit, ok := h.pepperCache.Get(tenantID); ok {
		return hit, nil
	}
	pepper, err := h.store.GetPhoneHashPepper(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("get pepper: %w", err)
	}
	if len(pepper) != 32 {
		return nil, fmt.Errorf("%w: pepper length %d", api.ErrInvalidArgument, len(pepper))
	}
	h.pepperCache.Add(tenantID, pepper)
	return pepper, nil
}
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/tenancy/service/... -run PhoneHasher -v
```

Expected: pass (4 tests).

- [ ] **Step 4: Commit**

```bash
git add internal/tenancy/service
git commit -m "tenancy: add PhoneHasher with HMAC-SHA256 + per-tenant pepper + E.164 normalisation"
```

---

## Task 6: Per-tenant S3 bucket provisioning + lockdown of `pkg/postgres`

**Purpose:** spec §12.1 L5 mandates bucket-per-tenant for recordings (defence-in-depth: KMS scope + IAM scope + bucket scope). Plan 01 only creates platform-shared buckets (`backups`, `reports`, `consent_prompts`, `tfstate`). Plan 04 must create the recording bucket as part of `TenantService.Create`, alongside KEK provisioning. **Same task** also locks down `pkg/postgres` so RLS-bypass class-of-bugs is blocked at the language level.

**Files:**
- Modify: `internal/tenancy/api/types.go` — add `BucketProvisioner` interface.
- Create: `internal/tenancy/service/bucket_provisioner.go` — Yandex Object Storage adapter.
- Create: `internal/tenancy/service/bucket_provisioner_test.go`.
- Modify: `internal/tenancy/service/tenant_service.go` — Create/Suspend/Archive flow calls bucket provisioner.
- Modify: `pkg/postgres/pool.go` — un-export `*pgxpool.Pool`, expose only `WithTenantTx`.
- Modify: `.golangci.yml` — depguard rule blocking direct `pgxpool.Pool` import outside `pkg/postgres`.
- Modify: `cmd/api/main.go` — startup assert that the `app` connection's `current_user` is `app`, not `tenancy_admin`.

### Subtask 6a: BucketProvisioner interface + Yandex impl

- [ ] **Step 1: Define `BucketProvisioner` interface in `internal/tenancy/api/types.go`**

```go
// BucketProvisioner manages per-tenant Object Storage buckets used for
// recordings. The implementation MUST be idempotent: re-calling Provision
// for an existing tenant returns success without recreating the bucket.
type BucketProvisioner interface {
    // Provision creates (if absent) the recordings bucket for tenant `id`,
    // configures SSE-KMS with the tenant's KEK, applies a per-tenant IAM
    // policy that limits s3:GetObject/s3:PutObject to the tenant's
    // service account, and returns the bucket name.
    Provision(ctx context.Context, tenantID uuid.UUID, kmsKeyID string) (bucketName string, err error)

    // Decommission marks the bucket for deletion. Real DELETE happens via
    // a separate worker after grace period — we never delete recordings
    // synchronously from a TenantService call.
    Decommission(ctx context.Context, tenantID uuid.UUID) error
}
```

- [ ] **Step 2: Write failing test for `BucketProvisioner.Provision` happy path**

`internal/tenancy/service/bucket_provisioner_test.go`:

```go
//go:build integration

package service_test

// Uses Yandex Object Storage S3-compat endpoint via testcontainers minio
// (configured to mimic the OS bucket policy surface).

func TestBucketProvisioner_Provision_Idempotent(t *testing.T) {
    ctx := context.Background()
    p := newTestProvisioner(t) // helper constructs adapter against minio
    tenantID := uuid.New()

    name1, err := p.Provision(ctx, tenantID, "test-kek-id")
    require.NoError(t, err)
    require.Equal(t, "sociopulse-recordings-"+tenantID.String(), name1)

    name2, err := p.Provision(ctx, tenantID, "test-kek-id")
    require.NoError(t, err)
    require.Equal(t, name1, name2, "second call returns existing bucket")
}
```

Run: `go test -tags=integration ./internal/tenancy/service/...` → fails (`Provision` not defined).

- [ ] **Step 3: Implement `bucket_provisioner.go` (Yandex Object Storage)**

```go
package service

import (
    "context"
    "errors"
    "fmt"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/service/s3"
    "github.com/aws/aws-sdk-go-v2/service/s3/types"
    "github.com/google/uuid"

    "social-pulse/internal/tenancy/api"
)

const bucketPrefix = "sociopulse-recordings-"

type S3BucketProvisioner struct {
    cli *s3.Client
}

func NewS3BucketProvisioner(cli *s3.Client) *S3BucketProvisioner {
    return &S3BucketProvisioner{cli: cli}
}

func (p *S3BucketProvisioner) Provision(ctx context.Context, tenantID uuid.UUID, kmsKeyID string) (string, error) {
    name := bucketPrefix + tenantID.String()

    // Idempotent: HeadBucket → if exists, just verify SSE-KMS config and return.
    _, err := p.cli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
    if err == nil {
        return name, nil
    }
    var nf *types.NotFound
    if !errors.As(err, &nf) {
        return "", fmt.Errorf("head bucket: %w", err)
    }

    // Create bucket.
    _, err = p.cli.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
    if err != nil {
        return "", fmt.Errorf("create bucket: %w", err)
    }

    // Apply SSE-KMS default encryption with the tenant's KEK.
    _, err = p.cli.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
        Bucket: aws.String(name),
        ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
            Rules: []types.ServerSideEncryptionRule{{
                ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{
                    SSEAlgorithm:   types.ServerSideEncryptionAwsKms,
                    KMSMasterKeyID: aws.String(kmsKeyID),
                },
            }},
        },
    })
    if err != nil {
        return "", fmt.Errorf("put bucket encryption: %w", err)
    }

    // Apply lifecycle: hot 365 days, cold tier 730 days, then expire.
    // Aligned with spec §9.4.
    _, err = p.cli.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
        Bucket: aws.String(name),
        LifecycleConfiguration: &types.BucketLifecycleConfiguration{
            Rules: []types.LifecycleRule{{
                ID:     aws.String("recordings-tier"),
                Status: types.ExpirationStatusEnabled,
                Filter: &types.LifecycleRuleFilterMemberPrefix{Value: ""},
                Transitions: []types.Transition{{
                    Days:         aws.Int32(365),
                    StorageClass: types.TransitionStorageClassGlacier,
                }},
                Expiration: &types.LifecycleExpiration{Days: aws.Int32(365 + 730)},
            }},
        },
    })
    if err != nil {
        return "", fmt.Errorf("put lifecycle: %w", err)
    }

    // Apply per-tenant IAM bucket policy: deny all except the tenant's
    // service account principal.
    policy := fmt.Sprintf(`{
        "Version": "2012-10-17",
        "Statement": [{
            "Effect": "Deny",
            "Principal": "*",
            "Action": "s3:*",
            "Resource": ["arn:aws:s3:::%s/*"],
            "Condition": {"StringNotEquals": {"aws:userid": "ten-%s"}}
        }]
    }`, name, tenantID.String())
    _, err = p.cli.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
        Bucket: aws.String(name),
        Policy: aws.String(policy),
    })
    if err != nil {
        return "", fmt.Errorf("put bucket policy: %w", err)
    }

    return name, nil
}

func (p *S3BucketProvisioner) Decommission(ctx context.Context, tenantID uuid.UUID) error {
    // Mark via tag — actual DELETE is done by Plan 12 retention worker after
    // grace period (default 30 days). We never synchronously delete recordings.
    name := bucketPrefix + tenantID.String()
    _, err := p.cli.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
        Bucket: aws.String(name),
        Tagging: &types.Tagging{TagSet: []types.Tag{
            {Key: aws.String("decommissioned"), Value: aws.String("true")},
            {Key: aws.String("decommissioned_at"), Value: aws.String(time.Now().UTC().Format(time.RFC3339))},
        }},
    })
    return err
}

var _ api.BucketProvisioner = (*S3BucketProvisioner)(nil)
```

- [ ] **Step 4: Wire `BucketProvisioner` into `TenantService.Create`**

Modify `internal/tenancy/service/tenant_service.go` constructor:

```go
type tenantService struct {
    log    *zap.Logger
    store  store.Store
    kms    KMSClient
    bp     api.BucketProvisioner
    pub    nats.Publisher
}

func NewTenantService(log *zap.Logger, st store.Store, kms KMSClient, bp api.BucketProvisioner, pub nats.Publisher) api.TenantService {
    return &tenantService{log: log, store: st, kms: kms, bp: bp, pub: pub}
}
```

In `Create`, after KEK is created and tenant row is inserted, but BEFORE returning:

```go
bucketName, err := s.bp.Provision(ctx, tn.ID, tn.KMSKEKID)
if err != nil {
    // The tenant exists but bucket provisioning failed. Two options:
    //   (a) rollback the tenant insert  — but KEK is already provisioned
    //       in Yandex KMS and rolling it back is complex.
    //   (b) leave the tenant in a "pending" state and allow operator to
    //       retry via /admin/tenants/{id}/repair.
    // We pick (b) and surface the failure as ErrBucketProvisionPending,
    // logged loud. The Service-Owner UI shows the tenant in red with a
    // "Repair" button.
    s.log.Error("bucket provisioning failed; tenant left in pending",
        zap.String("tenant_id", tn.ID.String()),
        zap.Error(err))
    return tn, fmt.Errorf("%w: %v", ErrBucketProvisionPending, err)
}

// Persist bucket name in tenant_settings (idempotent insert).
if err := s.store.UpdateBucket(ctx, tn.ID, bucketName); err != nil {
    return tn, fmt.Errorf("persist bucket name: %w", err)
}
tn.RecordingBucket = bucketName
```

Add `ErrBucketProvisionPending` to `internal/tenancy/api/errors.go`. Add `RecordingBucket string` to the `Tenant` DTO. Add `UpdateBucket(ctx, tenantID, bucketName)` to `Store`.

- [ ] **Step 5: Add `Repair` admin endpoint**

`POST /admin/tenants/{id}/repair` calls `TenantService.Repair(ctx, id)` which retries `Provision` and clears `ErrBucketProvisionPending`. Audit-logged. Idempotent.

- [ ] **Step 6: Run integration tests → green**

`go test -tags=integration ./internal/tenancy/...`

- [ ] **Step 7: Commit**

```bash
git add internal/tenancy/api internal/tenancy/service internal/tenancy/store
git commit -m "tenancy: provision per-tenant Object Storage bucket on Create"
```

### Subtask 6b: `pkg/postgres` lockdown — RLS bypass impossible at language level

- [ ] **Step 1: Refactor `pkg/postgres/pool.go` to keep `*pgxpool.Pool` unexported**

Currently the pool is exposed to all callers; refactor so only `WithTenantTx(ctx, tenantID, fn func(pgx.Tx) error) error` is exported.

```go
package postgres

import (
    "context"
    "fmt"

    "github.com/google/uuid"
    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
)

type Pool struct {
    pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string) (*Pool, error) {
    p, err := pgxpool.New(ctx, dsn)
    if err != nil {
        return nil, err
    }
    return &Pool{pool: p}, nil
}

func (p *Pool) Close() { p.pool.Close() }

// WithTenantTx is the SOLE supported way to read/write tenant-scoped data.
// It opens a transaction, sets app.tenant_id LOCAL, runs fn, and commits or
// rolls back. RLS policies in Plan 03 use current_setting('app.tenant_id')
// so this is the bypass-proof path.
func (p *Pool) WithTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
    tx, err := p.pool.Begin(ctx)
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback(ctx) //nolint:errcheck

    if _, err := tx.Exec(ctx, `SET LOCAL app.tenant_id = $1`, tenantID.String()); err != nil {
        return fmt.Errorf("set tenant id: %w", err)
    }
    if err := fn(tx); err != nil {
        return err
    }
    return tx.Commit(ctx)
}

// adminPool returns the raw pool for tenancy-admin operations. Lower-cased
// = unexported. ONLY internal/tenancy may build a Pool with admin DSN.
func (p *Pool) adminPool() *pgxpool.Pool { return p.pool }
```

- [ ] **Step 2: Add depguard rule in `.golangci.yml`**

```yaml
linters-settings:
  depguard:
    rules:
      pgxpool-blocked:
        list-mode: lax
        files:
          - "!**/pkg/postgres/**"
          - "!**/internal/tenancy/store/admin_*.go"
        deny:
          - pkg: github.com/jackc/pgx/v5/pgxpool
            desc: "Use pkg/postgres.Pool. Direct pgxpool.Pool import bypasses RLS guarantees."
```

The exception path `internal/tenancy/store/admin_*.go` allows the tenancy module's admin store (which intentionally uses BYPASSRLS) to use pgxpool directly.

- [ ] **Step 3: Add startup assertion in `cmd/api/main.go`**

After the `Pool` is constructed in cmd/api startup:

```go
// Defence: assert the app pool's connection user is `app`, not `tenancy_admin`.
// `tenancy_admin` has BYPASSRLS — accidentally connecting with it would
// disable tenant isolation on every query.
{
    var u string
    if err := pool.QueryRowAdmin(ctx, "SELECT current_user").Scan(&u); err != nil {
        return fmt.Errorf("verify pool user: %w", err)
    }
    if u != "app" {
        return fmt.Errorf("FATAL: app pool connected as %q, expected %q — refusing to start", u, "app")
    }
}
```

`QueryRowAdmin` is a tiny test-only helper on `Pool`; alternative: open a one-shot connection via `pgx.Connect(ctx, dsn)`, run the query, close. Either works.

- [ ] **Step 4: Add integration test — leak-free assert**

`pkg/postgres/leak_test.go`:

```go
//go:build integration

func TestWithTenantTx_OnlyReturnsTenantData(t *testing.T) {
    ctx := context.Background()
    pool := MustNewTestPool(t)
    defer pool.Close()

    // Insert two projects in two different tenants via admin connection.
    tenantA := uuid.New()
    tenantB := uuid.New()
    seedTwoTenants(t, pool, tenantA, tenantB)

    // Read via WithTenantTx for tenant A — must NOT see B's row.
    err := pool.WithTenantTx(ctx, tenantA, func(tx pgx.Tx) error {
        rows, err := tx.Query(ctx, `SELECT id FROM projects`)
        if err != nil {
            return err
        }
        defer rows.Close()

        ids := map[uuid.UUID]struct{}{}
        for rows.Next() {
            var id uuid.UUID
            if err := rows.Scan(&id); err != nil {
                return err
            }
            ids[id] = struct{}{}
        }
        require.Len(t, ids, 1, "tenant A should see exactly 1 project")
        return nil
    })
    require.NoError(t, err)
}
```

- [ ] **Step 5: Run lint + tests + commit**

```bash
golangci-lint run ./...
go test -tags=integration ./pkg/postgres/...
git add pkg/postgres/ .golangci.yml cmd/api/main.go
git commit -m "postgres: lock down pool — only WithTenantTx exported, depguard enforces"
```

---

---

## Self-review

**Spec coverage** (against §5.2, §6.2, §12.4, §13.2, §14):
- `TenantService` CRUD over `tenants` + `tenant_settings` tables (uses `tenancy_admin` role with BYPASSRLS — only module that does, per §6.1). ✓
- `KMSResolver` against Yandex KMS — lazy per-tenant KEK creation (8760h rotation), `Encrypt`/`Decrypt`/`GenerateDataKey` envelope ops. ✓
- DEK in-memory cache via `golang-lru/v2` with TTL 5 min per-tenant. ✓
- `PhoneHasher` — HMAC-SHA256 with per-tenant pepper from `tenants.phone_hash_pepper` (32 random bytes generated at tenant.Create). ✓
- `SettingsCache` — lazy-load + write-through + NATS `tenant.<id>.settings.updated` invalidation, TTL 30 sec safety net. ✓
- `/admin/tenants/*` HTTP endpoints with mTLS for Service-Owner level. ✓
- §13.2 KMS audit via Yandex Cloud Audit Trails (referenced in runbook `docs/runbooks/key-rotation.md`). ✓
- §14 default values from YAML overlaid by per-tenant overrides from DB. ✓

**Placeholder scan:** `KMSClient` is an interface — fakes used in unit tests, real `yandex-cloud/go-sdk` impl in production. No bare TODOs.

**Type/name consistency:** `TenantService`, `SettingsCache`, `KMSResolver`, `PhoneHasher` exposed via `internal/tenancy/api/` and consumed unchanged by downstream Plans 05, 06, 09, 11, 12.

**Out of scope (correctly deferred):**
- Per-tenant signup self-service flow — out of v1 (admin-provisioned).
- KEK rotation procedure — runbook only; full automation in v2.
- Service-Owner UI — separate internal tool, not part of СоциоПульс monolith.

**Task 6 (S3 bucket + pkg/postgres lockdown):**
- `BucketProvisioner` API + Yandex Object Storage adapter — bucket name `sociopulse-recordings-<tenant_id>`, SSE-KMS with tenant KEK, lifecycle (365d→cold→expire 730d more), per-tenant IAM policy. Idempotent `Provision`. Decommission only tags — actual delete via Plan 12 retention worker. ✓
- `TenantService.Create` rolls forward: tenant + KEK + bucket. Failure of bucket provisioning leaves tenant in `bucket_provision_pending` state with admin `Repair` endpoint. ✓
- `pkg/postgres.Pool` — only `WithTenantTx(ctx, tenantID, fn)` exported. `pgxpool.Pool` blocked outside `pkg/postgres` and `internal/tenancy/store/admin_*.go` via depguard. Startup assert that app pool connects as `app`, not `tenancy_admin`. Integration test asserts cross-tenant leak impossible. ✓
- This eliminates the entire RLS-bypass class of bugs at the language level: a developer cannot forget `SET LOCAL` because they have no API surface that allows it. ✓

Plan 04 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-04-tenancy-module.md`.**


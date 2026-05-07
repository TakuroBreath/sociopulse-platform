# 01. Package Layout and Naming

This document is the rulebook for *where a file goes* and *what it is
called*. Every directory in the tree below has one and only one
responsibility; mixing them is a review-blocking violation. The Go module
path is `github.com/sociopulse/platform` — older drafts that say
`social-pulse/...` are stale and must not be copied.

## Top-level Tree

```
sociopulse-platform/
├── cmd/                           # one subdirectory per binary
│   ├── api/main.go                # full monolith composition root
│   ├── worker/main.go             # asynq workers (background jobs)
│   ├── migrator/main.go           # `golang-migrate` runner
│   ├── telephony-bridge/main.go   # ESL ↔ NATS sidecar
│   ├── recording-uploader/main.go # filesystem watcher → S3 + gRPC commit
│   ├── synthetic/main.go          # standalone canary
│   └── status-page/main.go        # Alertmanager-API reader
│
├── internal/                      # private to this Go module
│   ├── modules/                   # Module interface, DI seam (Plan 02)
│   ├── gateway/                   # cross-cutting HTTP middleware (Plan 02)
│   ├── auth/                      # one directory per business module
│   ├── tenancy/
│   ├── crm/
│   ├── surveys/
│   ├── telephony/
│   ├── dialer/
│   ├── realtime/
│   ├── recording/
│   ├── analytics/
│   ├── reports/
│   ├── billing/
│   └── audit/
│
├── pkg/                           # reusable, project-wide utilities
│   ├── postgres/                  # pgx + RLS-bound pool wrapper
│   ├── outbox/                    # transactional outbox helper
│   ├── encryption/                # AES-GCM stream codec
│   ├── httputil/                  # gin error envelope, idempotency adapter
│   ├── grpc/                      # mTLS server/client constructors
│   ├── passwords/                 # argon2id PHC encoder
│   ├── obs/                       # zap logger factory, OTel init, metric registry
│   ├── clock/                     # clockwork wrapper for deterministic tests
│   └── ...
│
├── migrations/                    # golang-migrate SQL files
├── configs/                       # YAML defaults per env (development, staging, production)
├── deployments/                   # Helm charts, Argo CD apps, Packer / Ansible
├── tests/                         # cross-module integration + e2e (Playwright)
├── docs/                          # architecture, ADRs, plans, specs
├── web/                           # Vite + React + TS frontend (lives in this repo)
├── go.mod                         # module github.com/sociopulse/platform
├── go.sum
├── .golangci.yml                  # 35 linters + depguard module-isolation
├── Makefile                       # lint, test, test-cover, sqlc-generate, ...
└── CLAUDE.md, CONTRIBUTING.md, README.md
```

## `cmd/<binary>/main.go` — the Composition Root

`main.go` is **the only place** allowed to import concrete implementations
across modules. Its job is mechanical: load config, build dependencies,
call `module.Register(deps)` for each module that participates in this
binary, attach signal handlers, and block on shutdown.

A composition root looks like this (sketch — see Plan 02 for the real one):

```go
package main

import (
    "context"
    "os/signal"
    "syscall"

    "go.uber.org/zap"

    "github.com/sociopulse/platform/internal/modules"
    "github.com/sociopulse/platform/internal/auth"
    "github.com/sociopulse/platform/internal/tenancy"
    "github.com/sociopulse/platform/pkg/obs"
    "github.com/sociopulse/platform/pkg/postgres"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
    defer stop()

    cfg := mustLoadConfig()
    log := obs.NewLogger(cfg.Service)
    defer log.Sync() //nolint:errcheck

    pgPool := mustConnectPostgres(ctx, cfg.Database.Postgres, log)
    defer pgPool.Close()

    deps := modules.NewDeps(modules.DepsParams{
        Logger: log,
        DB:     pgPool,
        Redis:  mustConnectRedis(ctx, cfg.Database.Redis, log),
        NATS:   mustConnectNATS(ctx, cfg.NATS, log),
        Clock:  cfg.Clock(),
    })

    register := []modules.Module{
        tenancy.New(),  // trunk first — others depend on it
        auth.New(),
        // ... etc
    }
    for _, m := range register {
        if err := m.Register(deps); err != nil {
            log.Fatal("module register", zap.String("module", m.Name()), zap.Error(err))
        }
    }

    runHTTPServers(ctx, cfg, deps, log)
    <-ctx.Done()
}
```

Rules for `main.go`:

1. **No business logic.** Push it into `internal/<module>/service/`.
2. **One file per binary.** If `main.go` exceeds ~300 lines, extract
   helpers into sibling `cmd/<binary>/server.go`, `wiring.go`, etc.
   Keep them in the same `package main`.
3. **`gocyclo` is relaxed for `cmd/.*/main.go`** in `.golangci.yml` —
   composition is intrinsically branchy; we don't refactor for the
   linter, but we do extract.
4. **Smoke tests only.** `cmd/<binary>/main_test.go` may verify
   `/healthz` returns 200 and SIGTERM completes shutdown within the
   grace period; deeper coverage belongs to the modules.

## `internal/<module>/` — One Directory per Bounded Context

Every business module follows the same skeleton. Deviations get rejected
in review.

```
internal/<module>/
├── doc.go                 # package overview: responsibility, links to spec/plan
├── module.go              # implements internal/modules.Module
│
├── api/                   # PUBLIC surface — the only package other modules import
│   ├── doc.go
│   ├── <interface>.go     # one file per major interface (service.go, runtime.go, ...)
│   ├── dto.go             # cross-module DTOs (Project, Survey, Claims, ...)
│   ├── errors.go          # var ErrXxx = errors.New("module: description")
│   └── events.go          # NATS subject constants, asynq task type IDs
│
├── service/               # business logic — implements api/ interfaces
│   ├── <feature>.go       # one file per logical sub-feature
│   ├── <feature>_test.go  # unit tests, t.Parallel(), table-driven
│   └── module.go          # constructors + Deps struct used by Register()
│
├── store/                 # persistence adapters
│   ├── pg.go              # pgx implementations
│   ├── pg_test.go         # //go:build integration — testcontainers Postgres
│   ├── redis.go           # go-redis adapters (queue, cache, presence)
│   └── queries.sql        # sqlc input where applicable
│
├── http/                  # gin handlers (REST API) — ADR-0014
│   ├── handlers.go        # func (h *Handler) Foo(c *gin.Context)
│   ├── handlers_test.go   # gin.SetMode(gin.TestMode) + httptest
│   ├── routes.go          # exported func Mount(r *gin.RouterGroup, ...)
│   ├── dto.go             # request/response JSON shapes (NOT api/ DTOs)
│   └── errors.go          # error → status mapping
│
├── grpc/                  # gRPC service implementations (only recording, telephony)
│   ├── server.go
│   ├── server_test.go     # bufconn-based unit test
│   └── proto/v1/          # generated stubs go elsewhere — keep handlers here
│
└── events/                # NATS publishers and subscribers
    ├── publisher.go       # thin wrapper over nats.JetStreamContext
    ├── subscriber.go      # consumer setup, idempotent handlers
    └── *_test.go          # using nats-server/v2 in-process
```

A few clarifications that come up often:

- **`api/` is a leaf package in the dependency DAG.** It must not import
  `service`, `store`, `http`, `grpc`, or `events` from the same module.
  This keeps `api/` cheap to depend on.
- **`service/` may import `store/`** but only inside the same module. To
  consume another module's data, call its `api/` interface — never reach
  into `internal/<other>/store`.
- **`http/` and `grpc/` import `service/` for their handlers**, not via
  `api/`. They are part of the same module so the boundary is internal.
  When we want to expose handlers as plug-ins (Plan 02 `gateway` works
  this way), we expose a `Mount(r *gin.RouterGroup, deps Deps)` function.
- **`events/` is the only place that knows NATS subject strings**. The
  raw subject literals also appear as constants in `api/events.go` so
  cross-module subscribers (e.g. `realtime`) can refer to them without
  importing `events/`.

## `pkg/<utility>/` — Reusable Across Modules

Anything that has no business meaning and could be lifted into another Go
project verbatim goes in `pkg/`. Today's inhabitants:

| Package | What it does |
|---|---|
| `pkg/postgres` | pgxpool wrapper that runs `SET LOCAL app.tenant_id` per transaction. The only place `pgxpool.Pool` is reachable from non-`tenancy` code (depguard rule). |
| `pkg/outbox` | Transactional outbox: write event row in same TX as state change, relay to NATS in a background goroutine. Used by `dialer`, `recording`, `audit`, `tenancy`. |
| `pkg/encryption` | AES-256-GCM stream codec — wraps a DEK around an `io.Reader`. `recording` decodes audio chunks; `tenancy` runs the small-PII path. |
| `pkg/httputil` | gin error envelope, idempotency middleware adapter (stdlib → `gin.HandlerFunc`), request-id middleware. |
| `pkg/grpc` | `NewMTLSServer` and `NewMTLSClient` with cert pinning helpers. Used by `cmd/api` (recording server, telephony control) and `cmd/recording-uploader` (recording client). |
| `pkg/passwords` | argon2id PHC string encode/decode. |
| `pkg/obs` | zap logger factory, OTel tracer/meter init, Prometheus registry, redaction encoder. |
| `pkg/clock` | `clockwork.Clock` wrapper exposed to deterministic tests. |
| `pkg/middleware/auth` | gin middleware that consumes the `auth.api.ClaimsValidator` interface to validate a Bearer token; stores the resulting `Claims` opaquely in `*gin.Context` for downstream handlers to read via interface methods. |

Rules for `pkg/`:

1. **No business types** (`Project`, `Tenant`, `Claims`). Those live in
   `internal/<module>/api/`. **Exception:** `pkg/middleware/*` may
   thread domain types through `*gin.Context` as opaque values — it
   never reaches into a type's internals, only reads interface methods.
   Many projects keep HTTP middleware in `pkg/` for the same reason.
2. **No imports from `internal/`.** `pkg/` is below `internal/` in the
   dependency DAG, full stop. If you find yourself reaching upward,
   either invert the dependency or move the helper into the module.
3. **Tested in isolation.** `pkg/<utility>/<utility>_test.go`, `t.Parallel()`,
   table-driven, ≥80% coverage (`04-testing-strategy.md`).
4. **Documented.** Every `pkg/<utility>/doc.go` opens with one paragraph
   stating what the package is, what it deliberately is *not*, and which
   skill from `samber/cc-skills-golang` informs its style.

## What Goes in `internal/` vs `pkg/`

The decision tree:

1. Does it carry domain meaning? → `internal/<module>/`.
2. Could a different team in a different repo use it as-is? → `pkg/`.
3. Both? Split it: keep the generic part in `pkg/`, the domain-specific
   wrapper in `internal/<module>/`.
4. Unsure? → `internal/`. We can always promote later; demoting is
   harder because `pkg/` has wider exposure.

## Naming Conventions

| Element | Rule | Examples |
|---|---|---|
| Go package name | lowercase, single word, no underscores, no plural for collections | `auth`, `realtime`, `crm` (NOT `Auth_API`, `surveysapi`) |
| Directory under `internal/` | matches the package name in the leaf directory; nested dirs may be multi-word | `internal/auth/api`, `internal/recording/grpc` |
| File name | `snake_case.go`. One concept per file. Tests `<file>_test.go` next to the file. | `jwt_issuer.go`, `jwt_issuer_test.go` |
| Type name | `CamelCase`, exported when crossing the package boundary | `Authenticator`, `OperatorFSM`, `CallQueueItem` |
| Interface name | a noun (`Hub`, `Runtime`) or `<Subject>Service` (`UserService`, `RecordingService`) or `<Action>er` (`Dialer`, `Hasher`, `Renderer`) | — |
| Sentinel error | `ErrXxx`, lowercased message starting with `module: ` | `ErrInvalidCredentials`, `ErrTenantMismatch`, message `"auth: invalid credentials"` |
| Typed error | `XxxError` struct ending in `Error`, satisfies `Error() string` | `ValidationError`, `OopsError` |
| Receiver name | one or two letters, consistent across methods of the type | `func (s *Service) Foo(...)`, `func (h *Handler) Bar(...)` |
| Constructor | `New<Type>(deps...)` returning the **concrete** type, never the interface | `func NewAuthenticator(...) *Authenticator` |
| Compile-time check | `var _ api.X = (*concrete.X)(nil)` near every adapter that implements an `api/` interface | enforces the contract at build time |

A handful of recurring traps and how we avoid them:

- **Pointer vs value receivers.** Use a pointer receiver when the type
  has any state (mutex, counter, pool) or any field worth more than a
  few words. Use a value receiver only for tiny, immutable DTOs.
  Mixing the two on the same type is a `revive` warning.
- **Interface placement.** Define interfaces *where they are consumed*,
  not where they are implemented. Concretely: `internal/auth/service`
  defines a `RefreshTokenStore` interface listing exactly the methods it
  calls; the `internal/auth/store` Redis adapter satisfies it without
  knowing the interface exists. The exception is `internal/<module>/api/`
  — those interfaces are intentional contract surfaces.
- **No premature interfaces.** A single-implementation interface in
  `service/` is dead weight. Wait for the second consumer or test mock
  before extracting. (The interfaces in `api/` are NOT premature — they
  are the cross-module seam.)
- **Constructors return concrete.** Returning `api.Authenticator` from
  `NewAuthenticator` makes test assertions on the concrete type
  awkward. Return `*Authenticator`; let the caller upcast.

## Tests in the Layout

Tests live next to the code they exercise, never in a separate directory:

- Unit tests: `internal/auth/service/jwt_issuer_test.go`.
- Integration tests: `internal/auth/store/refresh_store_test.go` with
  `//go:build integration` at the top, exercised via
  `go test -tags=integration ./...`.
- E2E (Playwright) tests live in `tests/e2e/` because they cross
  language and binary boundaries.
- Per-package `goleak.VerifyTestMain` lives in
  `internal/<module>/<sub>/main_test.go` for every package that spawns a
  goroutine in production code (`pkg/outbox`, `internal/dialer/service`,
  `internal/realtime/service`, `internal/telephony/...`,
  `internal/recording/service`).

See `04-testing-strategy.md` and `08-tdd-discipline.md` for the
full discipline.

## What Linters Enforce Here

`.golangci.yml` (Plan 00a Task 8) enforces the layout mechanically:

- **`depguard:module-boundaries`** — denies imports of
  `internal/<module>/{service,store,events}` from outside that module.
  The only allowed cross-module entry is `internal/<module>/api`.
- **`depguard:banned-third-party`** — fails the build if any package
  pulls in `chi`, `echo`, `fiber` (ADR-0014 locks gin).
- **`depguard:banned-stdlib`** — denies `math/rand`, `crypto/des`,
  `crypto/md5`, `crypto/sha1`, `crypto/cipher.NewCBCEncrypter` /
  `NewCBCDecrypter`. See `07-go-coding-standards.md` § Security.
- **`importas`** — pins canonical import aliases so codebase-wide search
  works. E.g. `authapi "github.com/sociopulse/platform/internal/auth/api"`.
- **`goimports.local-prefixes: github.com/sociopulse/platform`** — sorts
  module-local imports into their own block.

## Cross-references

- `00-overview.md` — module list and dependency graph.
- `02-module-contracts.md` — what lives inside each `api/` package.
- `04-testing-strategy.md` — coverage targets per layer.
- `07-go-coding-standards.md` § Linter Mapping — the linter table.
- ADR-0014 — gin chosen as HTTP router.
- ADR-0006 — `pkg/postgres` and the RLS / `SET LOCAL` mechanism.
- Spec §5 — module decomposition source of truth.

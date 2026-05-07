# Architecture Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Establish the architectural skeleton of the `sociopulse-platform` Go monorepo **before any business logic is written**. By the end of this plan, the entire repo compiles (`go build ./...` ✓), every cross-module contract is defined as an interface in `internal/<module>/api/`, every shared infrastructure abstraction has a `pkg/` package, every binary in `cmd/` has a scaffolded `main.go`, and depguard rules forbid common architectural violations. Plans 02-14 then **fill in implementations** of these contracts — they don't invent new ones.

**Architecture:** Hexagonal / Clean Architecture in Go style.
- **Domain core**: `internal/<module>/api/` — interfaces + DTOs + sentinel errors. Has no dependencies on infrastructure (DB, HTTP, NATS).
- **Service layer**: `internal/<module>/service/` — implementation of `api/` interfaces. Pure business logic. Depends on `api/` of own and other modules, plus `pkg/` abstractions.
- **Infrastructure adapters**: `internal/<module>/store/` (DB), `internal/<module>/http/` (REST), `internal/<module>/grpc/` (gRPC), `internal/<module>/events/` (NATS).
- **Cross-cutting**: `pkg/` — reusable abstractions (postgres pool, outbox, encryption, observability, config, eventbus).
- **Entry points**: `cmd/<binary>/main.go` — composition root that wires implementations together.

Module dependency rule: **`internal/A/*` may import `internal/B/api/`** but never `internal/B/service/`, `internal/B/store/`, etc. Modules talk only through public contracts. Enforced by `depguard`.

**Tech Stack:** Go 1.22+, golangci-lint with depguard, mockery v2 for interface mocks (test-only).

**Spec sections covered:** §5 (module decomposition), §6 (data model interfaces), §10 (real-time contracts), §12 (security boundaries), §14 (configuration registry), §15 (observability conventions). All 13 ADRs from §17 promoted to standalone files.

**Prerequisites:**
- Plan 00 completed (Go module initialised, Makefile, golangci-lint, GitHub Actions CI, hello-world `cmd/api` with `/healthz`).

**What this plan does NOT do (intentionally):**
- No business logic implementation. Every method body is `panic("not implemented: see Plan NN")`.
- No HTTP routing wiring. No real gRPC servers. No NATS subscribers. Plan 02 wires composition root.
- No tests of behaviour — only "does this compile?" smoke tests.

**Execution order**: this plan runs **after Plan 00, before Plan 02**. After completing it, `cmd/api/main.go` still prints hello-world; the architecture is just laid out. Plan 02 then fills `cmd/api/main.go` with real composition.

---

## File Structure

After Plan 00a completes, the repo looks like:

```
sociopulse-platform/
├── go.mod, go.sum
├── Makefile
├── .golangci.yml                                    # depguard rules added
├── README.md
├── CLAUDE.md
├── CONTEXT.md                                       # NEW: domain glossary
├── docs/
│   ├── adr/                                         # NEW: 13 ADR files
│   │   ├── README.md                                # ADR index
│   │   ├── 0001-modular-monolith.md
│   │   ├── 0002-self-hosted-freeswitch.md
│   │   ├── 0003-progressive-dialer.md
│   │   ├── 0004-aes-256-gcm-application-level.md
│   │   ├── 0005-recording-integrity-99-5.md
│   │   ├── 0006-pgbouncer-transaction-mode.md
│   │   ├── 0007-freeswitch-outside-k8s.md
│   │   ├── 0008-survey-runtime-wasm-or-ts-fallback.md
│   │   ├── 0009-handwritten-css.md
│   │   ├── 0010-postgres-plus-clickhouse.md
│   │   ├── 0011-nats-over-kafka.md
│   │   ├── 0012-zap-over-slog.md
│   │   └── 0013-clickhouse-row-policies.md
│   ├── architecture/                                # NEW: design docs
│   │   ├── 00-overview.md
│   │   ├── 01-package-layout.md
│   │   ├── 02-module-contracts.md
│   │   ├── 03-error-handling.md
│   │   ├── 04-testing-strategy.md
│   │   ├── 05-configuration.md
│   │   ├── 06-observability.md
│   │   └── 07-go-coding-standards.md                 # samber/cc-skills-golang distilled
│   └── agents/                                      # already from setup-matt-pocock-skills
│       ├── issue-tracker.md
│       ├── triage-labels.md
│       └── domain.md
├── cmd/                                             # binary scaffolds
│   ├── api/main.go                                  # extends Plan 00 hello-world
│   ├── telephony-bridge/main.go                     # NEW
│   ├── recording-uploader/main.go                   # NEW
│   ├── migrator/main.go                             # NEW
│   ├── worker/main.go                               # NEW
│   ├── synthetic/main.go                            # NEW
│   └── status-page/main.go                          # NEW
├── internal/                                        # NEW: 12 module skeletons
│   ├── auth/api/{interfaces.go, dto.go, errors.go, events.go}
│   ├── tenancy/api/...
│   ├── crm/api/...
│   ├── surveys/api/...
│   ├── telephony/api/...                            # bridge contracts
│   ├── dialer/api/...
│   ├── realtime/api/...
│   ├── recording/api/...
│   ├── analytics/api/...
│   ├── reports/api/...
│   ├── billing/api/...
│   ├── audit/api/...
│   └── modules/                                     # registry pattern
│       └── module.go                                # Module interface, Deps struct
├── pkg/                                             # NEW: shared abstractions
│   ├── postgres/{pool.go, tx.go}
│   ├── outbox/{event.go, writer.go, relay.go, publisher.go}
│   ├── encryption/{aes.go, hasher.go, dek.go}
│   ├── observability/{logger.go, tracer.go, meter.go, middleware.go}
│   ├── config/{config.go, loader.go}
│   ├── eventbus/{publisher.go, subscriber.go}
│   ├── grpc/{server.go, client.go}                  # mTLS helpers
│   └── httputil/{requestid.go, recover.go, idempotency.go, ratelimit.go}
└── tests/                                           # placeholder, real tests in module dirs
    └── README.md
```

---

## Tasks

### Task 1 — Architecture decision documents

**Goal:** Write the seven design documents under `docs/architecture/`. These are the single source of truth for HOW the codebase is organised. They MUST be read by every agent before touching any other plan.

**Files:**
- Create: `docs/architecture/00-overview.md`
- Create: `docs/architecture/01-package-layout.md`
- Create: `docs/architecture/02-module-contracts.md`
- Create: `docs/architecture/03-error-handling.md`
- Create: `docs/architecture/04-testing-strategy.md`
- Create: `docs/architecture/05-configuration.md`
- Create: `docs/architecture/06-observability.md`
- Create: `docs/architecture/07-go-coding-standards.md`

- [ ] **Step 1: `00-overview.md` — module map + dependency graph**

Write the high-level architecture document. Sections:

1. **Bird's-eye view** — the platform is a Go modular monolith with two sidecars (telephony-bridge, recording-uploader) and a few worker binaries. All Go code lives in this monorepo.

2. **Module list** — table of 12 modules with their one-line responsibilities (copy from spec §5):

   | Module | Owns | Depends on (api/) |
   |---|---|---|
   | `auth` | sessions, JWTs, TOTP, RBAC matrix | `tenancy` (Resolve), `audit` (Log) |
   | `tenancy` | tenants, KMS, settings, phone hasher, bucket provisioning | `audit` |
   | `crm` | projects, respondents, quotas, DNC, imports | `tenancy`, `audit` |
   | `surveys` | survey schemas, DSL evaluator, runtime | `tenancy`, `audit` |
   | `telephony` | telephony-bridge sidecar contracts (ESL, Router) | `tenancy` |
   | `dialer` | OperatorFSM, CallQueue, RDD, retry orchestrator | `crm`, `surveys`, `telephony`, `tenancy`, `audit` |
   | `realtime` | WebSocket Hub, presence, listen-in | `auth`, `tenancy`, `dialer`, `audit` |
   | `recording` | gRPC Commit, S3 streaming, retention | `tenancy`, `audit` |
   | `analytics` | ClickHouse ingest, queries, materialised views | `tenancy` |
   | `reports` | preset + custom reports, async exports | `tenancy`, `analytics`, `audit` |
   | `billing` | cost calc, tariffs, monthly reports | `tenancy`, `analytics` |
   | `audit` | append-only audit log | (no internal deps) |

3. **ASCII dependency graph** — show that arrows point only from upstream to downstream, no cycles. `audit` is the leaf. `tenancy` is the trunk most others depend on.

4. **Binary mapping** — which modules are wired into which binary:
   - `cmd/api`: all modules (full monolith).
   - `cmd/worker`: `auth, tenancy, crm, recording, billing` (background jobs).
   - `cmd/migrator`: just runs SQL migrations, no domain modules.
   - `cmd/telephony-bridge`: only `telephony` module + ESL infrastructure (separate binary because it talks to FreeSWITCH ESL).
   - `cmd/recording-uploader`: only `recording` module's gRPC client + filesystem infra (deployed as systemd-unit on FS-VMs).
   - `cmd/synthetic`: standalone canary, no domain modules.
   - `cmd/status-page`: standalone, reads Alertmanager API.

5. **External dependencies map** — what each module talks to outside Go process: PostgreSQL, Redis (Valkey), NATS, ClickHouse, S3, KMS, FreeSWITCH ESL.

- [ ] **Step 2: `01-package-layout.md` — directory rules**

Document the layout:

1. **`cmd/<binary>/main.go`** — composition root. Builds all dependencies, wires them, starts servers. Imports concrete implementations.

2. **`internal/<module>/`** — one directory per business module. Subdirectories:
   - `api/` — public contracts. Interfaces, DTOs, sentinel errors, event types. **Other modules import only from here.**
   - `service/` — implements `api/` interfaces. Business logic.
   - `store/` — persistence adapters (Postgres, Redis, ClickHouse). Implements internal storage interfaces from `api/` or `service/`.
   - `http/` — chi-router handlers (REST API).
   - `grpc/` — gRPC service implementations (only `recording`, `telephony`).
   - `events/` — NATS publishers and subscribers.
   - `module.go` — implements `internal/modules.Module` interface to register self into composition.
   - `doc.go` — top-level package docstring.

3. **`pkg/<utility>/`** — reusable, project-wide abstractions. No business logic. Examples: `pkg/postgres`, `pkg/outbox`, `pkg/encryption`. Importable from anywhere.

4. **Naming conventions**:
   - Go package names: lowercase, single word, no underscores. `package auth`, `package realtime`.
   - File names: `snake_case.go`. Test files `*_test.go`.
   - Type names: `CamelCase`. Interfaces are nouns or `<Subject>Service`/`<Action>er`.
   - Errors: sentinel `var ErrXxx = errors.New(...)` or typed `type XxxError struct{...}`.
   - Receivers: pointer for stateful types (`func (s *Service) ...`), value for immutable DTOs.
   - Constructor: `func New<Type>(deps...) *<Type>` returning concrete type. Caller can type-assert to interface if needed.

5. **What goes in `internal/` vs `pkg/`**: domain logic → `internal/`. Generic utilities (would be useful in another project) → `pkg/`. When in doubt: `internal/`.

- [ ] **Step 3: `02-module-contracts.md` — every public interface**

For each of 12 modules, document:
- One-paragraph responsibility.
- The list of public interfaces in `api/`.
- The list of public DTOs.
- The list of sentinel errors.
- The list of NATS event types it publishes/subscribes to (refer to spec §10.2 canonical naming).

This document is the **specification for Task 4** below. Concretely list e.g. for `auth`:

```
internal/auth/api/

Interfaces:
- AuthService
    Login(ctx, LoginRequest) (LoginResponse, error)
    Refresh(ctx, RefreshRequest) (LoginResponse, error)
    Logout(ctx, sessionID) error
    Verify2FA(ctx, Verify2FARequest) (LoginResponse, error)
    EnrollTOTP(ctx, userID) (EnrollTOTPResponse, error)
- ClaimsValidator
    Validate(ctx, accessToken) (Claims, error)
- PasswordHasher
    Hash(plain) (string, error)
    Verify(plain, hash) error

DTOs:
- LoginRequest, LoginResponse
- RefreshRequest
- Verify2FARequest
- Claims (TenantID, UserID, Roles, SessionID, ExpiresAt)
- EnrollTOTPResponse (Secret, OTPAuthURL, BackupCodes)

Errors:
- ErrInvalidCredentials, ErrAccountLocked, ErrSessionExpired
- ErrTOTPRequired, ErrTOTPInvalid
- ErrRefreshReplay (security event)

Events: none published; consumes none.
```

Repeat for all 12 modules. This document is ~600-800 lines — that's expected and necessary.

- [ ] **Step 4: `03-error-handling.md` — error policy**

1. **Sentinel errors per module** (`var ErrXxx = errors.New("module: description")`). All in `internal/<module>/api/errors.go`.
2. **Wrapping**: always `fmt.Errorf("context: %w", err)` to preserve chain. Log only at the outermost handler.
3. **Single Handling Rule**: log OR return, never both. Wrap with context as it bubbles up.
4. **Errors-as-values**: don't panic for expected conditions (auth failure, not found, validation error). Panic only for unrecoverable programmer errors (nil pointer, impossible state, broken invariant).
5. **gRPC errors**: use `status.Error(codes.X, msg)` at the gRPC boundary, map sentinel errors to gRPC codes in middleware.
6. **HTTP errors**: domain errors map to HTTP status codes in `pkg/httputil/error_handler.go`. Standard envelope: `{"error": {"code": "...", "message": "...", "details": {...}}}`.
7. **`samber/oops`** library used at the outermost layer (HTTP/gRPC/worker handlers) for structured logging with context.

- [ ] **Step 5: `04-testing-strategy.md`**

Document testing layers:
1. **Unit** — fast, no external deps. Run on every commit. Use `gomock`/`mockery` for `api/` interfaces.
2. **Integration** — `testcontainers-go` per test, ephemeral PG/Redis/NATS. `//go:build integration` tag, run on PR.
3. **E2E** — Playwright for frontend (separate repo). Backend integration tests cover flow up to NATS/HTTP boundary.
4. **Coverage targets**: ≥85% per `internal/<module>/service/`, ≥70% per `store/`, ≥60% per `http/`/`grpc/`.
5. **Race detector** required in CI (`go test -race`).
6. **Goroutine leak detection** with `goleak` in tests that spawn goroutines.
7. **Snapshot tests** for stable outputs (config rendering, dialplan XML, JSON serialisation).

- [ ] **Step 6: `05-configuration.md`**

Layered config:
1. **YAML** in `configs/<env>/config.yaml` — defaults committed to repo.
2. **Env vars** override YAML. Format: `SOCIOPULSE_DATABASE_DSN`, etc.
3. **Lockbox secrets** override env (in production, via External Secrets Operator).
4. **Hot-reload**: viper `WatchConfig` for non-secret fields. Secrets reloaded by SIGHUP.
5. **Tenant overrides**: per-tenant settings live in `tenant_settings` table, not in YAML. Schema in spec §14.

- [ ] **Step 7: `06-observability.md`**

Conventions:
1. **Logging fields** (always include if available): `tenant_id`, `user_id`, `request_id`, `trace_id`, `op_id`, `call_id`, `module`.
2. **Metrics naming**: `sociopulse_<module>_<metric>` (snake_case). Labels: `tenant_id` only on per-tenant counters, never high-cardinality (no `respondent_id`).
3. **Trace span names**: `<module>.<Service>.<Method>` (e.g. `auth.AuthService.Login`).
4. **PII redaction**: any field matching `phone|password|token` regex masked at zap encoder level. Tested in CI.

- [ ] **Step 8: `07-go-coding-standards.md` — distilled samber/cc-skills-golang**

This document distils the **`samber/cc-skills-golang`** community skill pack
(MIT, 12 skills under `~/.agents/skills/golang-*/`) into project-specific
standards. The skill pack itself is the authoritative source of rationale and
examples; this document is the project-specific *application* of those
standards. Updated whenever a new Go version is adopted or a skill version
bumps.

Sections (write each as 3–6 paragraphs of guidance, not just a bullet list):

1. **Skill pack inventory.** Table of the 12 skills with one-line summaries:
   `golang-concurrency`, `golang-context`, `golang-data-structures`,
   `golang-design-patterns`, `golang-error-handling`, `golang-grpc`,
   `golang-modernize`, `golang-safety`, `golang-security`,
   `golang-structs-interfaces`, `golang-testing`, `golang-troubleshooting`.
   Note which are user-invocable (`/golang-modernize`, `/golang-security`,
   `/golang-testing`, `/golang-troubleshooting`) and which auto-trigger by
   description match.

2. **Errors (`golang-error-handling`).** Wrap with `fmt.Errorf("ctx: %w", err)`.
   Inspect with `errors.Is` / `errors.As`. Sentinel errors live in
   `internal/<module>/api/errors.go` as `var ErrXxx = errors.New("module:
   description")`. **Single-handling rule**: an error is either logged or
   returned, never both — duplicate logs flood Loki/Grafana. **Low cardinality
   only** in the error string: variable data (tenant_id, call_id, request_id)
   goes into structured `slog.Attr` or `oops.With(...)`, never interpolated
   into the message string. `samber/oops` used at the outermost handler
   (HTTP/gRPC/worker) for stack-trace + structured-attrs production errors.
   gRPC errors mapped via `status.Errorf(codes.X, ...)` + `errdetails`. HTTP
   errors mapped in `pkg/httputil/error_handler.go` to a stable JSON envelope.

3. **Context (`golang-context`).** `ctx context.Context` is always the first
   parameter. Same `ctx` propagated through HTTP handler → service → store →
   external client; never `context.Background()` mid-chain. `defer cancel()`
   immediately after `WithCancel/WithTimeout/WithDeadline`. **Background work
   that must outlive the parent request** (audit log writes, outbox-relay
   publishes after commit, async fire-and-forget telemetry) uses
   `context.WithoutCancel` (Go 1.21+) — preserves trace-id values, drops
   cancellation. Never store ctx in a struct.

4. **Concurrency (`golang-concurrency`).** Every goroutine has a clear exit
   (ctx.Done / explicit done channel / WaitGroup). For worker pools use
   `errgroup.WithContext` + `SetLimit(n)` rather than hand-rolled semaphores
   (matches dialer Worker design). Channel ownership: only sender closes;
   specify direction (`chan<-`, `<-chan`). **No `time.After` in loops** —
   each call leaks a timer until it fires; reuse `time.NewTimer` + `Reset`.
   `goleak.VerifyTestMain` is mandatory in every Go package that spawns
   goroutines: `pkg/outbox`, `internal/dialer/service`,
   `internal/realtime/service`, `internal/telephony/...`,
   `internal/recording/service`. Race detector required: CI runs
   `go test -race -count=1 ./...`.

5. **Interfaces and structs (`golang-structs-interfaces`).** Small (1–3
   methods); compose larger contracts. Defined where consumed
   (`internal/<consumer>/...` imports `internal/<producer>/api/`). Constructors
   return concrete types (`*Service`), never interface — caller assigns to
   interface var if they want. Premature interfaces forbidden — extract when
   second consumer or test mock arrives. **Compile-time interface check**
   `var _ api.X = (*concrete.X)(nil)` mandatory near every adapter type that
   implements an `api/` interface — catches drift at build time.

6. **Safety (`golang-safety`).** Comma-ok type assertion only (enforced by
   `forcetypeassert` linter). Typed nil ≠ nil interface — return untyped `nil`
   when the nil case is intended. Never write to nil map. `defer` extracted
   from loops: bodies become a function with own `defer Close()`. Integer
   conversions bounds-checked (`int64 → int32` may silently wrap). Float
   comparison via epsilon, never `==`. **Zero-value design**: structs work
   without explicit initialisation — embed `noCopy` sentinel for types
   containing `sync.Mutex`/channels.

7. **Security (`golang-security`).** `crypto/rand` for tokens, session IDs,
   API keys, recording-DEK, password salts. `math/rand/v2` only for
   non-security randomness (jitter, sampling). `math/rand` (the v1 package)
   forbidden by depguard. AES-256-GCM for envelope encryption (recordings,
   Lockbox secrets, KMS DEK wrap). CBC/ECB/DES/MD5/SHA1 forbidden by depguard.
   Parameterized SQL only — string concat is a depguard violation. Secret
   comparison via `crypto/subtle.ConstantTimeCompare`. `gosec` linter +
   `govulncheck` in CI (Plan 00 Task 11).

8. **Modernize (`golang-modernize`).** Project targets Go 1.22+. Use:
   - `any` over `interface{}`
   - `min`/`max`/`clear` builtins
   - `range` over int (Go 1.22)
   - `slices`/`maps` packages over hand-rolled helpers
   - `cmp.Or` for default values
   - `sync.OnceValue`/`OnceFunc` over manual `sync.Once` patterns
   - `t.Context()` in tests (Go 1.24+, when CI Go version supports it)
   - `wg.Go()` (Go 1.25+, when adopted)
   - `errors.Join` for accumulating independent errors
   - `slog.Attr` builders (`slog.String`, `slog.Int64`, `slog.Group`) over
     interface-typed key/value pairs

   Loop variable shadow copies (`for _, x := range xs { go f(x) }`) — no
   longer needed since Go 1.22 fixed the semantics, but explicit copy still
   preferred in cross-iteration goroutines for readability.

9. **Testing (`golang-testing`).** Table-driven tests with named subtests
   (`t.Run(tt.name, ...)`). `t.Parallel()` mandatory in unit tests
   (paralleltest linter). `//go:build integration` build tag for
   testcontainers-go integration tests; run via
   `go test -tags=integration ./...`. testify is used as helpers (`require`,
   `assert`), not as a replacement for the standard library — failure
   reporting must include enough context to debug from the CI log alone.
   Mock interfaces, never concrete types — every `internal/<module>/api/`
   interface gets a `mockery`-generated mock under `internal/<module>/api/mocks/`.
   `goleak.VerifyTestMain` for goroutine-spawning packages. Coverage targets
   match `04-testing-strategy.md` (≥85% service, ≥70% store, ≥60% http/grpc).

10. **gRPC (`golang-grpc`).** Used in two places only: `internal/recording/grpc/`
    (recording-uploader → cmd/api Commit) and `internal/telephony/grpc/`
    (cmd/api → telephony-bridge Router). Health check service registered.
    `GracefulStop()` with timeout fallback. Reflection disabled in production
    (`SOCIOPULSE_GRPC_REFLECTION=false` default). Always `status.Errorf(codes.X,
    ...)` — raw `error` becomes `codes.Unknown` and breaks client retry. Sentinel
    `internal/<module>/api/errors.go` errors mapped to gRPC codes in a single
    interceptor per service. mTLS via `pkg/grpc.NewMTLSServer/Client`.

11. **Troubleshooting (`golang-troubleshooting`).** Reproduce before fix —
    failing test first, then code change. Race detector + `goleak` are
    diagnostic, not optional. `pprof` enabled on `cmd/api` admin port (auth
    required, separate listener) for production debugging. Delve in dev only.

12. **Linter mapping table.** Reference table — for each rule above, name the
    `golangci-lint` linter that mechanically enforces it (see Plan 00 Task 9
    for the canonical list):

    | Rule | Linter |
    |---|---|
    | `%w` for error wrapping | `errorlint` |
    | `errors.Is/As` over `==`/type assertion | `errorlint` |
    | Comma-ok type assertion | `forcetypeassert` |
    | HTTP request without context | `noctx` |
    | Context propagation through chain | `contextcheck` |
    | `t.Parallel()` in tests | `paralleltest` |
    | `t.Helper()` in helpers | `thelper` |
    | testify idioms | `testifylint` |
    | slog/zap key-value pairs correct | `loggercheck` |
    | exhaustive switch over enum | `exhaustive` |
    | Module isolation `internal/X/api/` only | `depguard:module-boundaries` |
    | `math/rand`, weak crypto | `depguard:banned-stdlib` |
    | Body close on `*http.Response` | `bodyclose` |
    | `rows.Close()` / `rows.Err()` | `sqlclosecheck`, `rowserrcheck` |
    | Security patterns (gosec rules) | `gosec` |
    | Vulnerability scan | `govulncheck` (CI job, not lint) |

- [ ] **Step 9: Commit**

```bash
git add docs/architecture/
git commit -m "docs(architecture): add 8 design documents (overview, layout, contracts, errors, testing, config, obs, go-coding-standards)"
```

---

### Task 2 — Promote spec ADRs to standalone files

**Goal:** the 13 ADRs in spec §17 are the authoritative architectural decisions. Promote each to its own file under `docs/adr/` so future ADRs can be added incrementally and `improve-codebase-architecture` skill can read them.

**Files:**
- Create: `docs/adr/README.md` (index + ADR template)
- Create: `docs/adr/0001-modular-monolith.md` ... `0013-clickhouse-row-policies.md`

- [ ] **Step 1: Write `docs/adr/README.md`**

Template + table of contents:

```markdown
# Architecture Decision Records

This directory contains ADRs in the [Nygard format](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions).

## Status

Each ADR is one of: **Proposed**, **Accepted**, **Deprecated**, **Superseded by ADR-NNNN**.

## ADR template

```markdown
# ADR-NNNN: <Title>

**Status:** Accepted | Proposed | Deprecated | Superseded by ADR-NNNN
**Date:** YYYY-MM-DD
**Decider:** <name>

## Context
What's the problem? What forces are at play?

## Decision
What did we decide?

## Alternatives considered
- A: ...
- B: ...

## Consequences
Positive, negative, neutral. What are the trade-offs?

## Related
- ADR-NNNN
- Spec §X.Y
```

## Index

| # | Title | Status |
|---|---|---|
| [0001](0001-modular-monolith.md) | Modular monolith over microservices | Accepted |
| [0002](0002-self-hosted-freeswitch.md) | Self-hosted FreeSWITCH cluster | Accepted |
| [0003](0003-progressive-dialer.md) | Progressive auto-dialer (1:1) | Accepted |
| [0004](0004-aes-256-gcm-application-level.md) | AES-256-GCM at application level for PII | Accepted |
| [0005](0005-recording-integrity-99-5.md) | Recording integrity SLO 99.5% | Accepted |
| [0006](0006-pgbouncer-transaction-mode.md) | PgBouncer transaction-mode + SET LOCAL RLS | Accepted |
| [0007](0007-freeswitch-outside-k8s.md) | FreeSWITCH outside Kubernetes | Accepted |
| [0008](0008-survey-runtime-wasm-or-ts-fallback.md) | Survey runtime: TinyGo→WASM with TS-port fallback | Conditional Accept |
| [0009](0009-handwritten-css.md) | Hand-written CSS over Tailwind | Accepted |
| [0010](0010-postgres-plus-clickhouse.md) | Postgres for OLTP + ClickHouse for OLAP | Accepted |
| [0011](0011-nats-over-kafka.md) | NATS JetStream over Kafka | Accepted |
| [0012](0012-zap-over-slog.md) | zap over slog for logging | Accepted |
| [0013](0013-clickhouse-row-policies.md) | ClickHouse row policies for tenant isolation | Accepted |
```

- [ ] **Step 2: Generate the 13 ADR files**

For each ADR in spec §17, create a separate `.md` file using the template. Copy the existing content (Context, Decision, Alternatives, Consequences) and reformat per the template above. Keep all dates as `2026-05-06`. ~50 lines per ADR.

- [ ] **Step 3: Commit**

```bash
git add docs/adr/
git commit -m "docs(adr): promote spec §17 ADRs to standalone files (13 ADRs)"
```

---

### Task 3 — Domain glossary `CONTEXT.md`

**Goal:** repo-root `CONTEXT.md` defines the domain language so AI agents and human readers use consistent terminology. This is the file that `grill-with-docs` skill maintains.

**Files:**
- Create: `CONTEXT.md`

- [ ] **Step 1: Write `CONTEXT.md`**

Sections:

1. **One-paragraph project description** — sociology call-centre platform, multi-tenant.

2. **Glossary** (alphabetical, ~20-30 terms):

   - **Abandonment** — call answered by respondent but not bridged to operator within timeout.
   - **AHT** — Average Handling Time, including dial + talk + wrap.
   - **Auto-dialer (Progressive 1:1)** — dial one number per ready operator (vs Predictive 2:1+).
   - **Call attempt** — single dialing event. One respondent has many call attempts.
   - **Consent prompt** — IVR audio played before recording starts (152-FZ requirement).
   - **DEK** — Data Encryption Key, per-recording AES-256, encrypted by KEK and stored alongside the object.
   - **DNC** — Do Not Call list. Numbers excluded from dialing.
   - **DSL (survey)** — domain-specific language for conditional branching in survey schemas.
   - **Envelope encryption** — encrypt data with DEK, encrypt DEK with KEK. Industry-standard pattern.
   - **ESL** — Event Socket Library, FreeSWITCH's RPC protocol.
   - **FSM (operator)** — Finite State Machine: offline → ready → dialing → call → status → verify → pause.
   - **JetStream** — NATS persistence layer with at-least-once delivery.
   - **KEK** — Key Encryption Key, per-tenant master key in Yandex KMS.
   - **Listen-in** — admin silently listens to live operator call.
   - **Operator** — call-centre worker who runs the survey with a respondent.
   - **Outbox pattern** — durable transactional queue: write to event_outbox table inside business transaction, separate relay publishes to NATS.
   - **PII** — Personally Identifiable Information (phone, full name).
   - **Project** — a survey campaign: one survey schema, target sample, quotas.
   - **Quota** — required count of respondents matching specific dimensions (region × gender × age).
   - **RDD** — Random Digit Dialing. Generate phone numbers algorithmically.
   - **Recording** — encrypted Opus audio of the operator-respondent conversation.
   - **Respondent** — person being surveyed.
   - **RLS** — Row-Level Security in PostgreSQL.
   - **Service-Owner** — platform-level admin (cross-tenant). Distinct from tenant admin.
   - **SIP-trunk** — connection from telephony provider for outbound calls.
   - **Sociology** — the project's business domain. Distinct from marketing/cold-calling (38-FZ).
   - **Survey schema** — versioned JSON describing questions, branching, validation.
   - **Tenant** — call-centre customer of the platform.
   - **TURN** — relay server for WebRTC NAT traversal.
   - **Verto** — FreeSWITCH WebRTC signaling protocol.
   - **WAS** — WebAssembly survey runtime; Go code compiled with TinyGo.

3. **Cross-references**: see ADRs (link to `docs/adr/`), see spec (link to `docs/superpowers/specs/`).

4. **Concepts NOT in this domain** — explicitly note things the system DOES NOT do, to prevent scope creep:
   - Email marketing
   - SMS campaigns (out of v1)
   - Cold-calling regulated by 38-FZ
   - CRM-style ticket management
   - Video conferencing

- [ ] **Step 2: Commit**

```bash
git add CONTEXT.md
git commit -m "docs: add CONTEXT.md domain glossary"
```

---

### Task 4 — Module API contracts (12 modules)

**Goal:** create `internal/<module>/api/` for each of 12 modules with interfaces, DTOs, sentinel errors, and event types. Method bodies are NOT here (api/ has no implementation). After this task, `go build ./...` is green even though no service does anything.

**Files** (per module):
- `internal/<module>/api/interfaces.go`
- `internal/<module>/api/dto.go`
- `internal/<module>/api/errors.go`
- `internal/<module>/api/events.go` (only for modules that publish events)

**Style requirements** (apply to every adapter type created in downstream plans
02-14 — recorded here so reviewers reject violations early):

1. **Compile-time interface check.** Every adapter type that implements an
   `api.X` interface MUST include `var _ api.X = (*ConcreteType)(nil)` near the
   type declaration. This catches drift between contract and implementation at
   build time rather than at runtime. Source:
   `samber/cc-skills-golang@golang-structs-interfaces` § Compile-Time Interface
   Check.

   Example for a future plan: when Plan 03 introduces `internal/auth/store.UserStore`,
   the file MUST contain:
   ```go
   var _ api.UserStore = (*UserStore)(nil)
   ```

2. **Accept interfaces, return concrete types.** Constructors return
   `*ConcreteType`, never the interface — the caller can assign to interface
   variable if they need narrowed access. Source:
   `samber/cc-skills-golang@golang-structs-interfaces` § Accept Interfaces,
   Return Structs.

3. **No premature interfaces in `service/`.** If a `service` struct has a
   single implementation, it stays as a concrete type. Extract an interface
   only when (a) a second implementation appears, or (b) a test requires
   mocking. The `api/` package interfaces are the *consumer-facing* contract;
   internal `service/` types are concrete until proven otherwise. Source:
   `samber/cc-skills-golang@golang-structs-interfaces` § Don't Create Interfaces
   Prematurely.

4. **Errors in `api/errors.go` use low-cardinality strings.** Sentinel
   declaration: `var ErrTenantNotFound = errors.New("tenancy: tenant not found")`.
   Variable data (tenant_id, request_id) attaches via `slog.Attr` or
   `oops.With(...)` at the log site, never interpolated into the sentinel
   string. Source: `samber/cc-skills-golang@golang-error-handling` § Best
   Practice 15.

**Process:** for each module, follow this 4-step pattern. Below I give the full pattern for `auth` as a worked example. For the other 11 modules, derive the contracts from the corresponding plan (see "Source plan" column) using the same pattern.

**Module → source plan mapping:**

| Module | Source plan(s) | Notes |
|---|---|---|
| `auth` | Plan 05 | Argon2id, JWT, TOTP, RBAC |
| `tenancy` | Plan 04 | TenantService, KMSResolver, PhoneHasher, SettingsCache, BucketProvisioner |
| `crm` | Plan 06 | ProjectService, RespondentService, QuotaTracker, DNCManager, ImportService |
| `surveys` | Plan 07 | SurveyService, SchemaValidator, Runtime |
| `telephony` | Plan 09 | ESLClient, Router, BackpressureChecker (interfaces only — implementation in cmd/telephony-bridge) |
| `dialer` | Plan 10 | OperatorFSM, CallQueue, RDDGenerator, WorkingHoursChecker, RetryOrchestrator |
| `realtime` | Plan 11 | Hub, ListenInService, PresenceTracker, TopicRBAC |
| `recording` | Plan 12 | RecordingService (gRPC + HTTP), RetentionPlanner, IntegrityVerifier |
| `analytics` | Plan 13 | AnalyticsService, IngestPipeline, MetricsQuery |
| `reports` | Plan 13 | ReportService (preset + custom + async exports) |
| `billing` | Plan 14 | CostCalculator, TariffStore, RevenueCalculator, MarginReport |
| `audit` | spec §13.6 + Plan 04 audit_log | AuditLogger (no internal deps — leaf module) |

#### Worked example: `internal/auth/api/`

- [ ] **Step A.1: Create `internal/auth/api/dto.go`**

```go
// Package api defines public contracts for the auth module.
// Other modules import from this package only; never from auth/service or auth/store.
package api

import (
    "time"

    "github.com/google/uuid"
)

// LoginRequest is what cmd/api receives at POST /api/auth/login.
type LoginRequest struct {
    Email    string
    Password string
    // ClientIP and UserAgent are populated by the HTTP handler from request metadata.
    ClientIP  string
    UserAgent string
}

// LoginResponse is the successful-login envelope. AccessToken expires in 15 min,
// RefreshToken in 30 days. If 2FA is required, AccessToken is empty and the
// caller must follow up with Verify2FA.
type LoginResponse struct {
    AccessToken    string
    RefreshToken   string
    ExpiresAt      time.Time
    Need2FA        bool
    Need2FAHandle  string // opaque token to pass back to Verify2FA
    MustChangePwd  bool
}

type RefreshRequest struct {
    RefreshToken string
}

type Verify2FARequest struct {
    Need2FAHandle string
    Code          string // 6-digit TOTP
}

// Claims are the parsed contents of an access JWT. Used by every other module
// to authorise tenant- and role-scoped operations.
type Claims struct {
    TenantID  uuid.UUID
    UserID    uuid.UUID
    SessionID uuid.UUID
    Roles     []string // e.g. ["operator", "supervisor"]
    ExpiresAt time.Time
}

type EnrollTOTPRequest struct {
    UserID uuid.UUID
}

type EnrollTOTPResponse struct {
    Secret      string   // base32 (for QR code)
    OTPAuthURL  string   // otpauth://...
    BackupCodes []string // 10 single-use codes; show ONCE
}
```

- [ ] **Step A.2: Create `internal/auth/api/errors.go`**

```go
package api

import "errors"

var (
    // ErrInvalidCredentials — wrong email/password. Generic to avoid enumeration.
    ErrInvalidCredentials = errors.New("auth: invalid credentials")

    // ErrAccountLocked — too many failed attempts; lockout in effect.
    ErrAccountLocked = errors.New("auth: account locked")

    // ErrSessionExpired — refresh token expired or revoked.
    ErrSessionExpired = errors.New("auth: session expired")

    // ErrTOTPRequired — first factor passed; awaiting Verify2FA.
    ErrTOTPRequired = errors.New("auth: TOTP code required")

    // ErrTOTPInvalid — wrong TOTP code.
    ErrTOTPInvalid = errors.New("auth: invalid TOTP code")

    // ErrRefreshReplay — refresh-token replay detected (security incident).
    // The auth service revokes the entire session lineage when this fires.
    ErrRefreshReplay = errors.New("auth: refresh token replay detected")

    // ErrPasswordChangeRequired — first-login flow demands password change before issuing full access token.
    ErrPasswordChangeRequired = errors.New("auth: password change required")
)
```

- [ ] **Step A.3: Create `internal/auth/api/interfaces.go`**

```go
package api

import (
    "context"

    "github.com/google/uuid"
)

// AuthService is the public auth surface. Implemented in internal/auth/service.
type AuthService interface {
    Login(ctx context.Context, req LoginRequest) (LoginResponse, error)
    Refresh(ctx context.Context, req RefreshRequest) (LoginResponse, error)
    Verify2FA(ctx context.Context, req Verify2FARequest) (LoginResponse, error)
    Logout(ctx context.Context, sessionID uuid.UUID) error
    EnrollTOTP(ctx context.Context, req EnrollTOTPRequest) (EnrollTOTPResponse, error)
    DisableTOTP(ctx context.Context, userID uuid.UUID, byAdmin uuid.UUID) error
    VerifyBackupCode(ctx context.Context, userID uuid.UUID, code string) error
    RegenerateBackupCodes(ctx context.Context, userID uuid.UUID) ([]string, error)
}

// ClaimsValidator parses and validates an access JWT. Used by HTTP and WS
// auth middleware in every other module. Stateless; safe to share.
type ClaimsValidator interface {
    Validate(ctx context.Context, accessToken string) (Claims, error)
}

// PasswordHasher abstracts Argon2id so tests can use a fast fake.
type PasswordHasher interface {
    Hash(plain string) (string, error)
    Verify(plain, hash string) error
}
```

- [ ] **Step A.4: `internal/auth/api/events.go` (auth has no events)**

```go
// Package api — auth module events.
// auth does not publish NATS events directly; security events go via audit module.
package api
```

- [ ] **Step A.5: Smoke compile**

```bash
go build ./internal/auth/...
```

Expected: success. No service or store under `internal/auth/` yet — `api/` is self-contained.

- [ ] **Step A.6: Commit**

```bash
git add internal/auth/
git commit -m "feat(auth/api): define module contracts (interfaces, DTOs, errors)"
```

#### Repeat for the other 11 modules

For each of `tenancy`, `crm`, `surveys`, `telephony`, `dialer`, `realtime`, `recording`, `analytics`, `reports`, `billing`, `audit`:

1. **Read the source plan** (column 2 of the table above) — locate every interface and DTO it defines under `internal/<module>/api/`.
2. **Create `dto.go`, `errors.go`, `interfaces.go`, `events.go`** following the auth pattern. Keep method signatures identical to the plan; do NOT invent new methods.
3. **Compile-check**: `go build ./internal/<module>/...` green.
4. **Commit** per module.

Specific notes per module:

- `tenancy`: define `TenantService`, `KMSResolver`, `PhoneHasher`, `SettingsCache`, `BucketProvisioner`. DTOs include `Tenant`, `CreateTenantRequest`, `Settings`. Errors: `ErrTenantNotFound`, `ErrBucketProvisionPending`. KMSClient is also defined here as an interface so tenancy doesn't depend on Yandex SDK directly.

- `crm`: `ProjectService`, `RespondentService`, `QuotaTracker`, `DNCManager`, `ImportService`. Lots of DTOs (`Project`, `Respondent`, `Quota`, `ImportJob`). Events: `crm.respondent.imported`, `crm.respondent.deletion_requested`.

- `surveys`: `SurveyService`, `SchemaValidator`, `Runtime`. Note: `Runtime` interface is shared between server (Go impl) and browser (WASM via TinyGo OR TS-port per ADR-008).

- `telephony`: this module is special — most of its implementation lives in `cmd/telephony-bridge` (separate binary). The `api/` package defines what other modules see: `BridgeClient` interface (for cmd/api → bridge gRPC calls) and event types (`telephony.event.*` NATS subjects).

- `dialer`: `OperatorFSM`, `CallQueue`, `RDDGenerator`, `WorkingHoursChecker`, `RetryOrchestrator`. The FSM state enum is here (`State` type with `StateOffline`, `StateReady`, ..., `StatePause`).

- `realtime`: `Hub`, `Connection`, `ListenInService`, `PresenceTracker`, `TopicRBAC`, `Topic`, `Frame`, `FrameClass`, `SubscriptionFilter`. Lots of types — this is the largest api/ package.

- `recording`: `RecordingService` (server-side gRPC contract), `RecordingClient` (uploader-side), `RetentionPlanner`, `IntegrityVerifier`. Note: gRPC proto file lives separately (`docs/api/recording/v1/recording.proto`); the Go interface here is the **server-side adapter**.

- `analytics`: `AnalyticsService`, `IngestPipeline`, `MetricsQuery`. ClickHouse-specific types are abstracted (`ClickHouseRow` interface).

- `reports`: `ReportService`, `ReportDefinition`, `ReportRow`, `AsyncExportJob`. Preset reports as constants.

- `billing`: `CostCalculator`, `TariffStore`, `RevenueCalculator`. Money type as `int64` копейки (per ADR — money never floats).

- `audit`: `AuditLogger` (only `Log(ctx, AuditEvent) error`). DTOs: `AuditEvent` with all standard fields. **Leaf module** — no `internal/<X>/api/` imports.

- [ ] **Step 5: Verify whole-repo compile**

```bash
go build ./...
```

Expected: all 12 modules' `api/` packages compile in isolation.

- [ ] **Step 6: Final commit**

After the per-module commits in Step 1-11 and the verify in Step 5, no extra commit needed.

---

### Task 5 — Shared `pkg/` abstractions

**Goal:** the cross-cutting infrastructure abstractions go in `pkg/` so they can be imported by anyone. Implementations are stubs that compile but `panic("not implemented")` at runtime — Plans 02, 03, 04 fill them in.

**Files** (per package):
- `pkg/<name>/<file>.go` with type definitions and stub implementations
- One `_test.go` file per package with a "compile-only" test
- For packages that will spawn goroutines (`pkg/outbox`, `pkg/eventbus`):
  `pkg/<name>/main_test.go` with `goleak.VerifyTestMain` (see Step 9 below)

- [ ] **Step 1: `pkg/postgres/`**

```go
// pkg/postgres/pool.go
package postgres

import (
    "context"
    "github.com/google/uuid"
    "github.com/jackc/pgx/v5"
)

// Pool wraps pgxpool.Pool to enforce that all tenant data access goes
// through WithTenantTx, which sets app.tenant_id LOCAL inside a transaction.
// The underlying pgxpool.Pool is intentionally unexported.
type Pool struct {
    // implementation in Plan 03 Task 4
}

// New is the constructor. DSN format: postgres://user:pass@host:port/db?sslmode=...
func New(ctx context.Context, dsn string) (*Pool, error) {
    panic("not implemented: see Plan 03 Task 4")
}

func (p *Pool) Close() {
    panic("not implemented: see Plan 03 Task 4")
}

// WithTenantTx is the only way to read/write tenant-scoped tables. It opens
// a transaction, sets app.tenant_id LOCAL to tenantID, runs fn, and commits or rolls back.
func (p *Pool) WithTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
    panic("not implemented: see Plan 03 Task 4")
}
```

- [ ] **Step 2: `pkg/outbox/`**

Stubs for `Event`, `Writer`, `PostgresWriter`, `Relay`, `Publisher`, `WithTx`. See Plan 03 Task 6 for full implementation.

- [ ] **Step 3: `pkg/encryption/`**

Stubs for `Encrypt`, `Decrypt`, `PhoneHasher`, `NormalizePhone`. See Plan 03 Task 5.

- [ ] **Step 4: `pkg/observability/`**

Stubs for `NewLogger`, `NewTracer`, `NewMeter`, `RequestIDMiddleware`, `LoggingMiddleware`, `TracingMiddleware`, `MetricsMiddleware`. See Plan 02 Task 2.

- [ ] **Step 5: `pkg/config/`**

Stubs for `Config` struct with all sub-sections (`Database`, `Redis`, `NATS`, `S3`, `KMS`, `Auth`, `Dialer`, `Telephony`, `Recording`, `Reports`, `Outbox`, `Observability`), and `Load(path) (*Config, error)`. See Plan 02 Task 1.

- [ ] **Step 6: `pkg/eventbus/`**

```go
// pkg/eventbus/publisher.go
package eventbus

import "context"

type Publisher interface {
    Publish(ctx context.Context, subject string, payload []byte) error
}

type Subscriber interface {
    Subscribe(ctx context.Context, subject string, handler func(subject string, payload []byte) error) error
}
```

NATS-specific implementation in Plan 02.

- [ ] **Step 7: `pkg/grpc/` and `pkg/httputil/`**

Stub helper functions: `NewMTLSServer`, `NewMTLSClient`, `IdempotencyMiddleware`, `RateLimitMiddleware`. Plan 02 fills.

- [ ] **Step 8: Add `go.uber.org/goleak` dependency**

```bash
go get go.uber.org/goleak@latest
```

Expected: `go.uber.org/goleak` added to `go.mod`. Reference:
[`samber/cc-skills-golang@golang-concurrency`](~/.agents/skills/golang-concurrency/SKILL.md)
§ Best Practice 9 — track goroutine leaks in tests.

- [ ] **Step 9: Add `goleak.VerifyTestMain` to packages that will spawn goroutines**

For each package that will spawn goroutines in its real implementation, create
a `main_test.go` with leak detection. Even though stubs panic, the file is in
place so the leak guard activates the moment Plan 02/03 fills in real code.

```go
// pkg/outbox/main_test.go
package outbox

import (
    "testing"

    "go.uber.org/goleak"
)

func TestMain(m *testing.M) {
    goleak.VerifyTestMain(m)
}
```

Required for: `pkg/outbox`, `pkg/eventbus`, `pkg/grpc`. (Other `pkg/*`
packages — `postgres`, `encryption`, `observability`, `config`, `httputil`
— have no goroutines and don't need this guard.)

The same `main_test.go` template will be added to every `internal/<module>/service/`
in Plans 09, 10, 11, 12 (telephony-bridge worker pool, dialer Worker pool,
realtime Hub goroutines, recording uploader). The skill's Common Mistakes
table flags fire-and-forget goroutines as the #1 mistake.

- [ ] **Step 10: Compile-check + commit**

```bash
go build ./pkg/...
go test -count=1 ./pkg/...    # only compile-smoke tests pass; goleak guard active
git add pkg/ go.mod go.sum
git commit -m "feat(pkg): scaffold shared abstractions + goleak guard for goroutine packages"
```

---

### Task 6 — `cmd/` binary scaffolds

**Goal:** every binary referenced in the plans has a `cmd/<name>/main.go` that compiles and runs (printing "not implemented yet" and exiting 0). Plans 02, 09, 12 fill them with real composition.

**Files:**
- `cmd/api/main.go` — already exists from Plan 00 (hello-world). Leave as-is. Plan 02 expands.
- `cmd/telephony-bridge/main.go` — NEW
- `cmd/recording-uploader/main.go` — NEW
- `cmd/migrator/main.go` — NEW
- `cmd/worker/main.go` — NEW
- `cmd/synthetic/main.go` — NEW
- `cmd/status-page/main.go` — NEW

- [ ] **Step 1: Each `main.go` follows the same pattern**

```go
// cmd/telephony-bridge/main.go
package main

import (
    "fmt"
    "os"
)

func main() {
    fmt.Fprintln(os.Stderr, "telephony-bridge: not implemented yet (see Plan 09)")
    os.Exit(0)
}
```

Same for the other 5 new ones, each pointing at its source plan.

- [ ] **Step 2: Verify all binaries build**

```bash
go build ./cmd/...
ls -la bin/  # if Makefile target exists
```

Or inline:

```bash
for d in cmd/*/; do
  go build -o /tmp/build-test ./$d && echo "$d ✓" || echo "$d ✗"
done
rm -f /tmp/build-test
```

Expected: all 7 binaries build.

- [ ] **Step 3: Commit**

```bash
git add cmd/
git commit -m "feat(cmd): scaffold all 7 binary entry points"
```

---

### Task 7 — Module registry + `Module` interface

**Goal:** the composition root in `cmd/api/main.go` (Plan 02) wires modules into chi-router, gRPC server, and NATS subscribers. To do this declaratively, define a `Module` interface that every module implements. Plan 02 walks a list of modules and calls `Register` on each.

**Files:**
- Create: `internal/modules/module.go`

- [ ] **Step 1: Define `Module` interface**

```go
// Package modules defines the registration pattern used by cmd/api to
// compose all business modules into running servers.
package modules

import (
    "context"

    "github.com/go-chi/chi/v5"
    "go.uber.org/zap"
    "google.golang.org/grpc"

    "social-pulse/pkg/config"
    "social-pulse/pkg/eventbus"
    "social-pulse/pkg/postgres"
)

// Deps is what every module receives at registration. It's the curated set
// of cross-cutting dependencies the composition root knows how to build.
type Deps struct {
    Ctx          context.Context
    Logger       *zap.Logger
    Config       *config.Config
    Pool         *postgres.Pool
    EventBus     eventbus.Publisher
    Subscriber   eventbus.Subscriber
    HTTPRouter   chi.Router
    GRPCServer   *grpc.Server
    Locator      ServiceLocator // for cross-module Service references
}

// ServiceLocator is the explicit registry for cross-module API references.
// Modules register their api.* implementations here at startup; downstream
// modules look them up by interface type.
//
// This pattern is used instead of compile-time DI to avoid cycles when two
// modules reference each other through interfaces.
type ServiceLocator interface {
    Register(name string, svc any)
    Lookup(name string) (any, bool)
}

// Module is implemented by each internal/<name>/module.go. cmd/api/main
// iterates the registry, calling Register on each module in dependency order.
type Module interface {
    Name() string
    Register(d Deps) error
}

// Registry holds the ordered list of modules to register.
type Registry struct {
    Modules []Module
}
```

- [ ] **Step 2: Add a stub `module.go` per `internal/<module>/`**

```go
// internal/auth/module.go
package auth

import "social-pulse/internal/modules"

type Module struct{}

func (Module) Name() string { return "auth" }

func (Module) Register(d modules.Deps) error {
    // Plan 05 Task 1 fills this in:
    //   1. Build store (internal/auth/store/)
    //   2. Build service (internal/auth/service.NewAuthService(store, ...))
    //   3. Register HTTP handlers on d.HTTPRouter
    //   4. Register service in d.Locator under "auth.AuthService"
    return nil
}
```

Repeat for all 12 modules. Each `module.go` is ~10 lines for now.

- [ ] **Step 3: Compile + commit**

```bash
go build ./...
git add internal/
git commit -m "feat(modules): add Module interface and per-module registration stubs"
```

---

### Task 8 — `.golangci.yml` depguard rules

**Goal:** mechanically enforce the architectural invariants documented in Task 1. Code that violates them fails CI before review.

**Files:**
- Modify: `.golangci.yml` (created in Plan 00)

- [ ] **Step 1: Add depguard rules**

Append to `.golangci.yml`:

```yaml
linters:
  enable:
    - depguard
    - errcheck
    - errorlint
    - gosimple
    - govet
    - ineffassign
    - misspell
    - revive
    - staticcheck
    - typecheck
    - unconvert
    - unused

linters-settings:
  depguard:
    rules:
      # Modules must talk only via api/. They may import their own internals freely.
      cross-module-isolation:
        list-mode: lax
        files:
          - "internal/*/service/**"
          - "internal/*/store/**"
          - "internal/*/http/**"
          - "internal/*/grpc/**"
          - "internal/*/events/**"
        deny:
          - pkg: "social-pulse/internal/auth/service"
            desc: "import only via internal/auth/api"
          - pkg: "social-pulse/internal/auth/store"
            desc: "import only via internal/auth/api"
          - pkg: "social-pulse/internal/tenancy/service"
            desc: "import only via internal/tenancy/api"
          - pkg: "social-pulse/internal/tenancy/store"
            desc: "import only via internal/tenancy/api"
          # ... repeat for all 12 modules' service/ and store/

      # pgxpool.Pool is reserved for pkg/postgres. Direct use risks RLS bypass.
      pgxpool-blocked:
        list-mode: lax
        files:
          - "!**/pkg/postgres/**"
          - "!**/internal/tenancy/store/admin_*.go"
        deny:
          - pkg: "github.com/jackc/pgx/v5/pgxpool"
            desc: "use pkg/postgres.Pool. Direct pgxpool.Pool import bypasses RLS."

      # Yandex SDK reserved for tenancy/store (KMS) and recording-uploader.
      yandex-sdk-isolation:
        list-mode: lax
        files:
          - "!**/internal/tenancy/store/**"
          - "!**/cmd/recording-uploader/**"
          - "!**/cmd/api/main.go"
        deny:
          - pkg: "github.com/yandex-cloud/go-sdk"
            desc: "Yandex SDK is provider-specific. Use abstractions in internal/tenancy/api."

      # Internal store/events implementations are reserved for their own module
      # plus cmd/* composition roots. Outside those, the api/ contract is the
      # only sanctioned import. Already configured in Plan 00 Task 9 — listed
      # here for completeness; depguard merges rules with the same name.
      cross-module-isolation:
        list-mode: lax
        files:
          - "internal/*/service/**"
          - "internal/*/store/**"
          - "internal/*/http/**"
          - "internal/*/grpc/**"
          - "internal/*/events/**"
        deny:
          # For every <module>, deny imports of every other <module>'s
          # service/, store/, http/, grpc/, events/. The 12-module list is
          # generated from the canonical module list — see Plan 00 Task 9 for
          # the auth/tenancy/crm/... entries; this section reuses that list.
          - pkg: "social-pulse/internal/auth/service"
            desc: "import only via internal/auth/api"
          - pkg: "social-pulse/internal/auth/store"
            desc: "import only via internal/auth/api"
          # ... full enumeration in Plan 00 Task 9 .golangci.yml block.

      # samber/cc-skills-golang@golang-concurrency § BP 8 — time.After in a
      # loop leaks one timer per iteration. Use time.NewTimer + Reset.
      # Allow time.After in test files and in `select` outside loops; we can't
      # easily express "outside loops" in depguard, so the rule lives in code
      # review + a CI grep guard instead. Keep this entry as a doc anchor:
      time-after-policy:
        list-mode: lax
        deny:
          # depguard cannot detect "in a loop"; this rule is intentionally a
          # no-op pkg entry to anchor the policy. Enforcement: a CI step greps
          # for `for ` followed by `time.After(` within 20 lines and fails the
          # build. See Plan 00 Task 11 — add a `make grep-time-after` target.
          - pkg: "this-is-a-doc-anchor-not-a-real-import"
            desc: "time.After in loops leaks timers — see samber/cc-skills-golang@golang-concurrency § BP8 + Common Mistakes table. Use time.NewTimer + Reset. CI greps for the pattern."

run:
  timeout: 5m

issues:
  exclude-dirs:
    - vendor
    - dist
```

- [ ] **Step 1b: Add `make grep-time-after` to Makefile + CI**

The `time.After`-in-loop pattern is hard to detect with depguard alone (it
needs syntactic context, not just import path). Add a fast grep guard.

In `Makefile` (created in Plan 00 Task 7):

```makefile
.PHONY: grep-time-after
grep-time-after: ## Fail if time.After appears within a for-loop scope
	@! grep -RnE --include='*.go' --exclude-dir=vendor 'for[ \t].*\{[^}]*time\.After\(' . \
	  || (echo 'time.After in loops leaks timers — use time.NewTimer + Reset (samber/cc-skills-golang@golang-concurrency § BP8)'; exit 1)
```

In `.github/workflows/ci.yml` (created in Plan 00 Task 11), add `make
grep-time-after` to the lint job. Approximate; refine after the first false
positive — CI greps are tradeoffs, not absolutes.

- [ ] **Step 2: Verify lint passes**

```bash
golangci-lint run ./...
```

Expected: zero issues. (The codebase is just scaffolds — no real violations possible yet.)

- [ ] **Step 3: Commit**

```bash
git add .golangci.yml
git commit -m "chore(lint): add depguard rules enforcing module isolation"
```

---

### Task 9 — Final compile gate + CI green

**Goal:** the whole repo compiles, all tests pass (only smoke tests exist), and CI is green. This is the "ready for Plan 02" checkpoint.

- [ ] **Step 1: Whole-repo build**

```bash
go build ./...
```

Expected: success.

- [ ] **Step 2: Whole-repo test**

```bash
go test ./...
```

Expected: pass (every test is "does this package compile" — no behaviour tested yet).

- [ ] **Step 3: Lint**

```bash
golangci-lint run ./...
```

Expected: clean.

- [ ] **Step 4: Push and verify CI**

```bash
git push origin main
```

Wait for GitHub Actions. CI must be green.

- [ ] **Step 5: Final commit (if any cleanup)**

If steps 1-3 surfaced issues, fix them in a single cleanup commit:

```bash
git add -A
git commit -m "chore: final cleanup for Plan 00a"
git push
```

---

## Self-review

**Spec coverage** (against §5, §6, §10, §12, §14, §15, §17):
- §5 module decomposition → 12 modules, each with `api/` package, dependencies match the spec table. ✓
- §17 ADRs → all 13 promoted to `docs/adr/` with index in README. ✓
- §10.2 NATS subjects → events.go in modules that publish events; subject names match spec. ✓
- §14 configuration → `pkg/config` Config struct with all sections. ✓
- §15 observability conventions → `docs/architecture/06-observability.md` + `pkg/observability` stubs. ✓
- §12 security boundaries → depguard rules enforce module isolation, pgxpool restriction, Yandex SDK isolation. ✓

**Placeholder scan:** every `pkg/` and `internal/<module>/api/` function body either compiles trivially (interface definition, type alias) or `panic("not implemented: see Plan NN")` with explicit pointer to the plan that fills it in. No bare TODOs.

**Type/name consistency:** the names defined here are referenced verbatim by Plans 02-14:
- `pkg/postgres.Pool`, `pkg/postgres.Tx`, `pkg/postgres.WithTenantTx`
- `pkg/outbox.Event`, `pkg/outbox.Writer`, `pkg/outbox.Relay`
- `pkg/encryption.Encrypt`, `pkg/encryption.Decrypt`, `pkg/encryption.PhoneHasher`
- `internal/auth/api.AuthService`, `ClaimsValidator`, `Claims`, `LoginRequest`, etc.
- `internal/tenancy/api.TenantService`, `KMSResolver`, `BucketProvisioner`, `SettingsCache`, `PhoneHasher`
- `internal/dialer/api.OperatorFSM`, `State` (enum), `CallQueue`, `RDDGenerator`
- `internal/realtime/api.Hub`, `Topic`, `Frame`, `FrameClass`, `SubscriptionFilter`, `ListenInService`
- `internal/recording/api.RecordingService`, `RetentionPlanner`, `IntegrityVerifier`
- `internal/modules.Module`, `Deps`, `ServiceLocator`, `Registry`

If a downstream plan introduces a name not in this scaffolding, that's a signal — either update Plan 00a's contracts to include it, or rename the downstream usage. No drift.

**Out of scope (correctly deferred):**
- Method bodies → Plans 02-14.
- Real config defaults (DSN, ports) → Plan 02.
- Database migrations → Plan 03.
- Helm charts → Plan 01 (Phase 2).
- ADR maintenance & new ADRs → ongoing as decisions arise. `improve-codebase-architecture` and `grill-with-docs` skills consume `docs/adr/` over time.

Plan 00a verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-00a-architecture-foundation.md`.**

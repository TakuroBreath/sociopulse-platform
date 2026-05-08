# Project Status — sociopulse-platform

> **Living document.** Updated at the end of every plan. Future agents read this first to know what exists and what's next.

**Last updated**: 2026-05-08 — Plan 05 (`v0.0.7-auth`) complete.

---

## Milestones (tags on origin)

| Tag | Plan | Date | What it gave us |
|---|---|---|---|
| `v0.0.1-foundation` | Plan 00 | 2026-05-06 | Go module init, Makefile, golangci-lint, GitHub Actions CI, hello-world cmd/api/`/healthz`, Dockerfile, Docker Hub publish to `maxtakuro/sociopulse-api`. |
| `v0.0.2-tech-baseline` | Plan 00b | 2026-05-06 | Go coding standards distilled from samber/cc-skills-golang, TDD discipline (ADR-0015), gin (ADR-0014), zap (ADR-0012), 35-linter golangci-lint with depguard module-boundaries. |
| `v0.0.3-architecture-foundation` | Plan 00a | 2026-05-07 | 8 architecture docs, 15 ADRs in `docs/adr/`, CONTEXT.md domain glossary, 12-module `internal/<X>/api/` contracts, 9 `pkg/` shared abstractions, internal/modules registry, 7 cmd/ binary scaffolds. |
| `v0.0.4-cmd-api-skeleton` | Plan 02 | 2026-05-07 | pkg/config (Viper + hot-reload), pkg/observability (zap+OTel+Prometheus + PII redaction), internal/healthz (live/ready), cmd/api full composition root, docker-compose.dev.yml. |
| `v0.0.5-database` | Plan 03 | 2026-05-07 | cmd/migrator (golang-migrate CLI), initial schema (19 tables + RLS), pkg/postgres (`WithTenant` SET LOCAL), pkg/encryption (AES-256-GCM + HMAC-SHA256 PhoneHasher), pkg/outbox (writer + relay + goleak guard). |
| `v0.0.6-tenancy` | Plan 04 | 2026-05-08 | internal/tenancy fully wired: TenantService CRUD via BypassRLS + outbox + audit; KMSResolver with LRU+TTL DEK cache; PhoneHasher per-tenant pepper resolver; BucketProvisioner (Yandex stub + Local in-memory); Go pinned to 1.26.3 (clears stdlib CVEs). |
| **`v0.0.7-auth`** | **Plan 05** | **2026-05-08** | **Full auth module across 9 sub-tasks: Argon2id (OWASP-min params + BoundedHasher OOM cap); HS256 JWT issuer/validator (refresh-rotation reuse detection, RFC 7518 32-byte secret floor); UserService CRUD (timing-safe ChangePassword, multi-role[]); Authenticator (login/refresh/logout, partial-token TOTP flow, dummy-verify on every failure path); Redis sliding-window rate-limit + lockout; TOTP enroll/verify/disable with KMS-encrypted secret + 10 single-use backup codes (cheap Argon2 params via `BackupCodeParams()`); static RBAC matrix (operator/supervisor/admin × 20 actions, multi-role union); gin handlers + JWTMiddleware replacing Plan 02 stub; full DI wired in `internal/auth/Module.Register`. Migration 000003 (users schema evolve) + 000004 (auth_totp).** |

---

## What exists right now

### Top-level
- `go.mod` module: `github.com/sociopulse/platform`, Go 1.26.1 source pins; CI/Dockerfile pin to **Go 1.26.3** (govulncheck-clean)
- `Makefile`: `lint/vet/test/build/run/clean/tools/grep-time-after/dev-up/dev-down/dev-psql/dev-redis-cli/dev-nats/dev-reset/migrate-up/migrate-down/migrate-status/migrate-create/...`
- `Dockerfile`: multi-stage, alpine-based, non-root, `/healthz` HEALTHCHECK
- `docker-compose.dev.yml`: PG/Redis/NATS/CH/MinIO with profiles `core/analytics/storage/full`
- `.golangci.yml`: 35 linters + depguard rules (module-boundaries, banned-stdlib, banned-third-party, pgxpool-isolation, yandex-sdk-isolation, time-after-policy doc anchor)
- `.gitleaks.toml`: secret-scan with docs allowlist
- `.github/workflows/ci.yml` (6 jobs: lint/test/build/docker/vuln/secret-scan) + `codeql.yml`
- `CLAUDE.md` (workflow rules + tooling notes) + `CONTEXT.md` (domain glossary) + `PROJECT_STATUS.md` (this document)

### `cmd/` (all 7 binaries compile)
- **`cmd/api/`** ✅ Production composition root: config load → logger/tracer/metrics → gin engine → middleware chain → /healthz/readyz/metrics → outbox relay (with noopPublisher; Plan 11 swaps NATS) → graceful shutdown. Adds `assertAppPoolUser` boot-time check that confirms the connection is the `app` role (not `tenancy_admin`) so RLS is in force.
- **`cmd/migrator/`** ✅ `up/down/status/force` subcommands; both `postgres://` and `pgx5://` DSN schemes.
- **`cmd/worker/`, `cmd/telephony-bridge/`, `cmd/recording-uploader/`, `cmd/synthetic/`, `cmd/status-page/`** 🟡 stubs (`exit 64 — not yet implemented`).

### `pkg/` (all unit tests green; integration tests via testcontainers)
- **`pkg/config/`** ✅ Full Viper loader: 16 sub-sections (Service/HTTP/WS/GRPC/Database/NATS/S3/KMS/Auth/Dialer/Telephony/Recording/Reports/Observability/Shutdown/Outbox), `atomic.Pointer[Config]` Snapshot, fsnotify hot-reload (CI-robust polling test), env override, seedDefaults covers every Validate-required field.
- **`pkg/observability/`** ✅ zap (PII redacting encoder), OTel tracer (OTLP/gRPC), Prometheus on isolated registry, gin middleware chain (RequestID→Logging→Tracing→Metrics).
- **`pkg/postgres/`** ✅ `Open/Close/Ping/WithTenant/BypassRLS/RawExec/RawQueryRow`. `WithTenant` uses `set_config('app.tenant_id', $1, true)` parameterized. RLS-verified via testcontainers.
- **`pkg/encryption/`** ✅ `Encrypt/Decrypt` (AES-256-GCM nonce-prefix), `PhoneHasher` (HMAC-SHA256 + per-tenant pepper), `NormalizePhone` (E.164 RU-aware passthrough — note: `internal/tenancy/service/phone_hasher.go` has its own STRICTER normaliser that rejects garbage upfront; the pkg one is more lenient).
- **`pkg/outbox/`** ✅ `PostgresWriter.Append`, `Relay.Run` (FOR UPDATE SKIP LOCKED + retry), `PublisherAdapter` (full-jitter exp backoff via `crypto/rand`), `goleak.VerifyTestMain`.
- **`pkg/eventbus/`** 🟡 interfaces only (Plan 11 wires real NATS).
- **`pkg/grpc/`** 🟡 stubs only (mTLS NewMTLSServer/Client — Plan 09/12).
- **`pkg/httputil/`** 🟡 stubs (RequestID/Recovery/Idempotency/RateLimit/ErrorEnvelope — Plan 02 wired the gin middleware path; httputil helpers still partially stubs).
- **`pkg/middleware/auth/`** ✅ Plan 05 — Real `JWTMiddleware`: extracts `Bearer <token>` (case-insensitive), calls `ClaimsValidator.Validate`, surfaces 401 `auth.token_invalid`/`auth.token_revoked`, stores Claims under `ClaimsContextKey`, exposes `ClaimsFromContext` helper. 97% coverage.
- **`pkg/passwords/`** ✅ Plan 05 — Argon2id PHC (`Hash`/`Verify`), `Hasher` interface (ctx-aware), `Default()` + `NewHasher(p)`, `BoundedHasher` (golang.org/x/sync/semaphore — caps in-flight Argon2 derivations to NumCPU so a login-burst can't OOM the pod). Two production param profiles: `DefaultParams()` (m=19 MiB, t=2, p=1 — OWASP min) and `BackupCodeParams()` (m=1 MiB, t=1, p=1 — for TOTP backup codes). 95%+ coverage.

### `internal/`
- **`internal/<module>/api/`** ✅ Contracts defined for 12 modules (auth, tenancy, crm, surveys, telephony, dialer, realtime, recording, analytics, reports, billing, audit).
- **`internal/tenancy/` ✅✅✅ Plan 04 — FULLY WIRED**:
  - `api/` — per-interface files: `doc.go`, `errors.go`, `tenant_service.go`, `kms.go`, `phone_hasher.go`, `settings.go`, `module.go`, `events.go`, `store.go`. Adds `Tenant.Validate()`, `TenantStatus.Valid()`, `Module`/`Deps`/`KMSClient` types, `api.Register` seam, `Tenancy` aggregate embedding 4 sub-interfaces directly. SettingsCache renamed `Get→Lookup` to avoid Get-method collision in aggregate. Sentinels: `ErrInvalidArgument`/`ErrNotFound`/`ErrAlreadyExists`/`ErrKMSUnavailable`/`ErrKEKNotFound`/`ErrInvalidWrappedDEK`/`ErrBucketProvisionPending`/`ErrBucketProvisionFailed`.
  - `store/` — `PostgresStore` (pgx-based CRUD), `LocalKMSClient` (in-process AES-256-GCM via pkg/encryption), `YandexKMSClient` build-tag stub (`//go:build yandex_kms`), `LocalBucketProvisioner` (in-memory dev/test), `YandexBucketProvisioner` build-tag stub (`//go:build yandex_s3`).
  - `service/` — `TenantService` (Insert+Suspend+Resume+Archive via `BypassRLS` tx + outbox.Append + audit stub), `KMSResolverImpl` (LRU+TTL cache, `(tenant_id, kek_version)` keying, ctx-aware lifecycle, plaintext zeroing best-effort), `PhoneHasher` (strict E.164 RU normalizer + HMAC-SHA256 + lazy LRU+TTL pepper cache), `eventbusPublisher` (NATS publisher adapter — currently no-op via cmd/api wiring), `Module.Register/Stop`. Wired into `modules.Locator` under `tenancy.TenantService`, `tenancy.KMSResolver`, `tenancy.PhoneHasher`.
  - `module.go` — composition root for tenancy: builds store, picks KMS provider (yandex|local) from config, picks bucket provisioner, registers all in Locator.
  - **120+ tenancy tests**, 30+ integration tests via testcontainers PG.
- **`internal/auth/` ✅✅✅ Plan 05 — FULLY WIRED**:
  - `api/` — `Authenticator`, `UserService`, `RBACChecker`, `SessionRevoker`, `TOTPService`, `TOTPVerifier`, `ClaimsValidator`, `JWTIssuer`, `UserStorePort`. DTOs: `Claims`, `AuthResult`, `User`, `LoginInput`, `LoginTOTPInput`, `CreateUserInput`, `ListUsersInput`, `TOTPEnrollment{Secret,OTPAuthURL,BackupCodes}`, `TOTPStatus`, `TOTPState`, `Action`, `Resource`. Sentinels covering credentials/lockout/archive/totp/token/refresh-replay/rate-limit/insufficient-role/login-taken/user-not-found/empty-roles/totp-already-enabled/totp-not-enrolled.
  - `service/` — `JWTIssuer` (HS256, RFC 7518 32-byte secret floor, leeway, alg-confusion guard, distinct JTI per token), `UserService` (CRUD with timing-safe ChangePassword via pre-baked dummy hash; constructor panics on nil deps), `Authenticator` (login/refresh/logout, dummy-verify on every failure path including per-account rate-limit, refresh-rotation reuse detection wires to `auth.refresh_replay` audit), `SessionRevoker` (per-sid kill keys + per-user cutoff with iat≤cutoff boundary), `RateLimiterRedis` (sliding window via ZADD/ZCARD pipeline, 30/IP/h + 10/user/h), `LockoutRedis` (5-fail threshold, 15min lock auto-unlock), `TOTPService` (KMS-encrypted secret, period=30, digits=6, skew=±1, 10 backup codes hashed with cheap Argon2), `RBACMatrix` (operator/supervisor/admin × 20 actions, multi-role union, 100% test coverage), `TenantResolverAdapter` (org_code → tenant_id via existing `tenancy.TenantService.GetByOrgCode`), `Metrics` (4 Prometheus collectors: login_success, login_failure{reason}, locked, refresh_replay).
  - `store/` — `UserStore` (pgx-based, unique-violation→ErrLoginTaken, supports ALL 9 UserService ops), `RefreshStore` (Redis whitelist + Lua atomic `Rotate` returning 3-way: not-found / already-rotated / success), `TOTPStore` (Postgres with RLS, `MarkBackupUsed` via array_remove for race-safety).
  - `transport/http/` — gin handlers for 17 endpoints: 4 public (login/login_totp/refresh/logout), 7 user-scoped (me / change_password / 4× totp), 6 admin (CRUD users / archive / restore / reset_password). `Mount(group, deps)` wiring; `requireRole(...)` middleware; thin handlers (bind→service→render); structured `mapAuthError` covering every sentinel; 87% coverage.
  - `module.go` — REAL composition root: BoundedHasher around Default; BackupCodeParams hasher; JWT issuer; three stores; four Redis services (refresh + revoker + ratelimit + lockout); UserService; TOTPService; Authenticator; RBACMatrix; TenantResolverAdapter; HTTP mount; six locator registrations; graceful audit-logger fallback to noop while internal/audit is still a stub.
- **`internal/<module>/api/` for the other 10 modules** ✅ Contracts only — no `service/`, `store/`, `http/`, `grpc/`, `events/` implementations yet (auth + tenancy are the two with real wiring as of v0.0.7).
- **`internal/healthz/`** ✅ `Liveness`/`Readiness` handlers + `Checker` interface + `PostgresCheck`/`RedisCheck`/`NATSCheck`.
- **`internal/modules/`** ✅ `Module` interface + `Deps` struct + `MapLocator` + `Registry`.
- Per-module `internal/<X>/module.go` ✅ stubs with `Register(d modules.Deps) error { return nil }` — all 12 modules (tenancy is the one with real wiring).

### `migrations/`
- `000001_init.up.sql` / `.down.sql` — 19 tables, 19 RLS policies, `tenancy_admin BYPASSRLS` role, `app` user. Plan 04 Task 2 added DML grants for `tenancy_admin` on `tenants` and `tenant_settings`.
- `000002_outbox.up.sql` / `.down.sql` — `event_outbox` table, indexes, owner=tenancy_admin.
- `000003_users_auth_evolve.up.sql` / `.down.sql` — Plan 05: ALTER `users` to add email/must_change_pwd/updated_at/created_by/archived_at/totp_enabled, replace `role text` → `roles text[]` (data-preserving), drop `status` (→ archived_at as soft-delete signal), drop `totp_secret_encrypted` (Plan 05 Task 6 owns auth_totp), idempotent RLS policy, three indexes (tenant+active, lower(login), email). Down migration includes a `RAISE EXCEPTION` guard against silent multi-role data loss on rollback.
- `000004_auth_totp.up.sql` / `.down.sql` — Plan 05 Task 6: `auth_totp` table (PK on user_id, FK to users + tenants ON DELETE CASCADE, RLS policy, partial index `WHERE enrolled`). Stores `secret_enc bytea` (KMS-wrapped per-tenant DEK) + `backup_codes_hash text[]` (Argon2id PHC strings, single-use via array_remove).

### `docs/`
- `architecture/00-overview.md` through `08-tdd-discipline.md` (8 design docs). Updated by Plan 04 Task 1 for SettingsCache rename.
- `adr/0001-...md` through `0015-...md` (15 ADRs + README index).
- `superpowers/specs/2026-05-06-sociopulse-system-design.md` (~2700 lines spec).
- `superpowers/plans/2026-05-06-NN-...md` (22 implementation plans).
- **`references/`** — per-plan curated reading lists. `README.md` (index + format), `COMMON.md` (cross-cutting: 152-ФЗ, Yandex Cloud, Go best practices, Postgres RLS, Outbox, NATS), `plan-05-auth.md` (ready), Plans 06-14/20 TBD. Subagent prompts include the file path so they read it before writing code. Future agents save time by not re-deriving canonical specs.

---

## Next plans (in dependency order)

### Plan 06 — CRM Module 🎯 **NEXT**
Projects, respondents (PII envelope-encrypted via pkg/encryption + per-tenant DEK from KMSResolver), DNC list, quotas, batch import. **Depends on Plan 04 + 05** ✅ both ready.

**Plan**: `docs/superpowers/plans/2026-05-06-06-crm-module.md`.

### Plan 07 — Surveys Module
Survey schema + DSL evaluator + WASM runtime (TinyGo per ADR-0008). **Depends on Plan 04**.

### Plan 08 — FreeSWITCH cluster (infra + Ansible). Mostly Plan 01 territory (parallel infra track).

### Plan 09 — telephony-bridge sidecar (ESL + Router + Backpressure).

### Plan 10 — dialer (OperatorFSM + CallQueue + RDD).

### Plan 11 — realtime (WebSocket Hub + ListenIn). Will swap cmd/api's noopPublisher for real NATS publisher.

### Plan 12 — recording (gRPC Commit + S3 streaming + retention). Will use `BucketProvisioner` for per-tenant buckets and KMSResolver to wrap recording DEKs.

### Plan 13 — analytics + reports (ClickHouse ingest + presets).

### Plan 14 — billing (cost calc + tariffs + monthly margin).

### Plan 01 — Infrastructure (parallel track, Yandex Cloud Terraform + Packer + Ansible + Helm). Plan 03 Task 1 (PgBouncer Helm chart) was deferred here. Plan 04 Task 6 stubs (Yandex KMS, Yandex S3) get real adapters when this lands.

### Plans 15-19 — Frontend (in `sociopulse-web` repo, NOT here).

### Plan 20 — Observability foundation (Prometheus/Grafana/Loki/Alertmanager).

---

## Standing rules / patterns

### TDD is mandatory (ADR-0015)
Every new function/method has a failing test FIRST. Watch it fail. Then minimal impl. See `docs/architecture/08-tdd-discipline.md` for Red-Green-Refactor canon. Reference: `superpowers:test-driven-development` skill.

### Skill discipline (samber/cc-skills-golang)
Loaded at `~/.agents/skills/golang-*/SKILL.md`. The 12 skills:
- `golang-concurrency` (BP1-BP9: goroutine exit, errgroup, NewTicker not After, goleak, channel direction)
- `golang-context` (ctx first param, WithoutCancel for outlive-parent, defer cancel)
- `golang-data-structures` (container/list LRU, sync.Map vs RWMutex+map)
- `golang-design-patterns` (enum-1: exhaustive switch)
- `golang-error-handling` (`%w` wrapping, sentinels, single-handling rule, low-cardinality strings)
- `golang-grpc` (status.Errorf with codes, mTLS, GracefulStop)
- `golang-modernize` (any over interface{}, range over int, slices/maps packages)
- `golang-safety` (comma-ok, no defer in loops, bounded conversions)
- `golang-security` (`crypto/rand`, AES-GCM, HMAC, parameterized SQL, ConstantTimeCompare, pepper/key plaintext NEVER logged)
- `golang-structs-interfaces` (small interfaces, defined where consumed, accept iface return struct, compile-time check)
- `golang-testing` (table-driven + t.Parallel, integration build tag, testify as helpers, goleak.VerifyTestMain)
- `golang-troubleshooting`

### Subagent-driven development (superpowers)
- Fresh implementer subagent per task (model: opus per user directive)
- Two-stage review: spec compliance, then code quality. Reviewer dispatches independent — never trust the implementer's report blindly.
- Reference: `superpowers:subagent-driven-development` skill.

### Path adaptation in older plans
Several plans say `internal/<X>` for things that ended up in `pkg/<X>` after Plan 00a:
- Plan 02 says `internal/config/` → actual is `pkg/config/`
- Plan 02 says `internal/observability/` → actual is `pkg/observability/`
- `internal/healthz/` is correct as-is (project-specific)
- `internal/tenancy/...` is correct (Plan 04 owns it)

When dispatching subagents, ALWAYS provide the path-correction note for older plans.

### gopls cache pollution
After subagent dispatches, gopls often shows stale errors (e.g., "undefined: X" when X is freshly defined in another file). Always re-verify with direct `go build ./... && go vet ./... && go test -race -count=1 ./...`. If those are green, the diagnostic is noise.

### Commit hygiene
- Every subagent must commit at the end of its task. Several subagents have "forgotten" — always check `git status` after the report and commit if uncommitted.
- Tag at end of each plan: `v0.0.N-<plan-slug>`.
- Push to origin/main, watch CI to green before tagging.

### CI Go version pin
`GO_VERSION: "1.26.3"` in `.github/workflows/ci.yml` and `Dockerfile`. Bump explicitly when stdlib CVEs surface — `1.26` alias resolves to whatever GitHub Actions has cached, which lags real releases.

### Stub-vs-real adapter pattern (KMS, Bucket)
For Yandex Cloud SDK adapters, the project uses a build-tag-gated stub pattern:
- Default build: `bucket_provisioner_yandex.go` is a stub returning explanatory error; tests cover validation only.
- `-tags=yandex_kms` / `-tags=yandex_s3`: real SDK-backed adapter (lands when Plan 01 infra is real).
- Local dev/test: `LocalKMSClient` / `LocalBucketProvisioner` (in-process, no external deps).

This keeps `go.sum` lean and CI fast. Real Yandex SDK only enters the build when explicitly requested.

### Docker Hub
- Image: `maxtakuro/sociopulse-api`
- Auto-push on every `main` commit via CI Docker job
- Public registry — no login to pull

### CodeQL note
The CodeQL workflow runs but Code Scanning isn't enabled in repo Settings → Code security. Failures are config-only, not code-level. Either enable Code Scanning in GitHub UI or accept CodeQL job failure as unrelated to code health.

### Hot-reload test on CI
`pkg/config.TestHotReloadReplacesSnapshot` was made CI-robust in Plan 04: poll `snap.Get()` instead of relying on a single subscriber-channel event. fsnotify on Linux fires spurious mid-write events; the snapshot is the source of truth.

### Tenancy-specific: the `Tenancy` aggregate
`internal/tenancy/api.Tenancy` is an interface that embeds 4 sub-interfaces directly: `TenantService + SettingsCache + KMSResolver + PhoneHasher`. The original spec used a method called `Get` on both TenantService and SettingsCache; the latter was renamed `Lookup` to avoid the collision. If you see plans referring to `SettingsCache.Get`, mentally substitute `SettingsCache.Lookup`. This rename is documented in `docs/architecture/02-module-contracts.md` and `05-configuration.md`.

### Tenancy-specific: cmd/api boots assertAppPoolUser
`cmd/api/postgres.go` has a startup check that confirms the pool's `current_user` is `app`, not `tenancy_admin`. This is a defence-in-depth: if someone misconfigures the DSN to use the admin role, the API refuses to boot rather than running with RLS bypassed.

### Tenancy-specific: pepper-at-rest is plaintext
The phone-hash pepper is stored as `bytea` in `tenants.phone_hash_pepper`. Plan 04 did NOT envelope-encrypt the pepper itself (would require a migration + storage refactor). Pragmatic stance — see compliance note below — this is acceptable for v1.

### Compliance posture
**Functional security, not compliance theater.** No external 152-ФЗ audit is planned in v1. We do AES-256-GCM, RLS, KMS, audit log, IVR consent because they're good engineering — not for regulators. Stop adding compliance ceremony unless an actual auditor surfaces. Rule documented in `CLAUDE.md` § Compliance posture and `docs/references/COMMON.md` § Compliance posture.

### Tooling rule (added 2026-05-08)
Subagents and the controller MUST use:
- **`context7` MCP** for library API verification (don't guess from training data).
- **`WebSearch`/`WebFetch`** for unknown errors and current documentation.
Wrong-API guesses cause subagent dispatch loops. Documented in `CLAUDE.md` § Tooling for unknown territory.

---

## Repo URL & identity

- Repo: https://github.com/TakuroBreath/sociopulse-platform
- Local git config: `TakuroBreath / maxsmurffy@gmail.com` (set via `git config --local`)
- `gh` CLI is authenticated as `TakuroBreath`
- Docker Hub repo: https://hub.docker.com/r/maxtakuro/sociopulse-api (PAT in GitHub Secrets `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN`)

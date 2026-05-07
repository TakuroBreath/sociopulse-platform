# Project Status — sociopulse-platform

> **Living document.** Updated at the end of every plan via `superpowers:grill-with-docs`. Future agents read this first to know what exists and what's next.

**Last updated**: 2026-05-07 — после Plan 03 (`v0.0.5-database`).

---

## Milestones (tags on origin)

| Tag | Plan | Date | What it gave us |
|---|---|---|---|
| `v0.0.1-foundation` | Plan 00 | 2026-05-06 | Go module init, Makefile, golangci-lint, GitHub Actions CI, hello-world cmd/api/`/healthz`, Dockerfile, Docker Hub publish to `maxtakuro/sociopulse-api`. |
| `v0.0.2-tech-baseline` | Plan 00b | 2026-05-06 | Go coding standards distilled from samber/cc-skills-golang, TDD discipline (ADR-0015), gin (ADR-0014), zap (ADR-0012), 35-linter golangci-lint with depguard module-boundaries. |
| `v0.0.3-architecture-foundation` | Plan 00a | 2026-05-07 | 8 architecture docs, 15 ADRs in `docs/adr/`, CONTEXT.md domain glossary, 12-module `internal/<X>/api/` contracts, 9 `pkg/` shared abstractions, internal/modules registry, 7 cmd/ binary scaffolds. |
| `v0.0.4-cmd-api-skeleton` | Plan 02 | 2026-05-07 | pkg/config (Viper + hot-reload), pkg/observability (zap+OTel+Prometheus + PII redaction), internal/healthz (live/ready), cmd/api full composition root, docker-compose.dev.yml. |
| `v0.0.5-database` | Plan 03 | 2026-05-07 | cmd/migrator (golang-migrate CLI), initial schema (19 tables + RLS), pkg/postgres (`WithTenant` SET LOCAL), pkg/encryption (AES-256-GCM + HMAC-SHA256 PhoneHasher), pkg/outbox (writer + relay + goleak guard). |

---

## What exists right now

### Top-level
- `go.mod` module: `github.com/sociopulse/platform`, Go 1.26.1
- `Makefile`: targets `lint/vet/test/build/run/clean/tools/grep-time-after/dev-up/dev-down/dev-psql/migrate-up/...`
- `Dockerfile`: multi-stage, alpine-based, non-root, `/healthz` HEALTHCHECK
- `docker-compose.dev.yml`: local PG/Redis/NATS/CH/MinIO with profiles
- `.golangci.yml`: 35 linters + depguard rules (module-boundaries, banned-stdlib, banned-third-party, pgxpool-isolation, yandex-sdk-isolation, time-after-policy doc anchor)
- `.gitleaks.toml`: secret-scan with docs allowlist
- `.github/workflows/ci.yml` (6 jobs: lint/test/build/docker/vuln/secret-scan) + `codeql.yml`
- `CLAUDE.md` (this file's rules) + `CONTEXT.md` (domain glossary)
- `PROJECT_STATUS.md` (this document)

### `cmd/` (all 7 binaries compile)
- **`cmd/api/`** — production composition root: config load → logger/tracer/metrics → gin engine → middleware chain → /healthz/readyz/metrics → outbox relay (with noopPublisher) → graceful shutdown
- **`cmd/migrator/`** — `up/down/status/force` subcommands; both `postgres://` and `pgx5://` DSN schemes
- **`cmd/worker/`, `cmd/telephony-bridge/`, `cmd/recording-uploader/`, `cmd/synthetic/`, `cmd/status-page/`** — stubs (`exit 64 — not yet implemented`)

### `pkg/` (all compile, all unit tests green, integration tests via testcontainers)
- **`pkg/config/`** ✅ Full Viper loader: 16 sub-sections (Service/HTTP/WS/GRPC/Database/NATS/S3/KMS/Auth/Dialer/Telephony/Recording/Reports/Observability/Shutdown/Outbox), atomic.Pointer Snapshot, fsnotify hot-reload, env override, seedDefaults covers every Validate-required field
- **`pkg/observability/`** ✅ zap (PII redacting encoder), OTel tracer (OTLP/gRPC), Prometheus on isolated registry, gin middleware (RequestID→Logging→Tracing→Metrics)
- **`pkg/postgres/`** ✅ `Open/Close/Ping/WithTenant/BypassRLS/RawExec/RawQueryRow`. `WithTenant` uses `set_config('app.tenant_id', $1, true)` parameterized. RLS-verified via testcontainers
- **`pkg/encryption/`** ✅ `Encrypt/Decrypt` (AES-256-GCM nonce-prefix), `PhoneHasher` (HMAC-SHA256 + per-tenant pepper), `NormalizePhone` (E.164 RU-aware)
- **`pkg/outbox/`** ✅ `PostgresWriter.Append`, `Relay.Run` (FOR UPDATE SKIP LOCKED + retry), `PublisherAdapter` (full-jitter exp backoff via `crypto/rand`), goleak.VerifyTestMain
- **`pkg/eventbus/`** 🟡 stubs only (Plan 04 will provide NATS impl)
- **`pkg/grpc/`** 🟡 stubs only (mTLS NewMTLSServer/Client — Plan 09/12)
- **`pkg/httputil/`** 🟡 stubs (RequestID/Recovery/Idempotency/RateLimit/ErrorEnvelope — Plan 02 partially wired in cmd/api)
- **`pkg/middleware/auth/`** 🟡 stub (JWT middleware — Plan 05)

### `internal/`
- **`internal/<module>/api/`** ✅ Contracts defined for 12 modules (auth, tenancy, crm, surveys, telephony, dialer, realtime, recording, analytics, reports, billing, audit). Each has `interfaces.go` + `dto.go` + `errors.go` + `events.go`. **No `service/`, `store/`, `http/`, `grpc/`, `events/` implementations yet.**
  - **`internal/tenancy/api/`** ✅✅ Plan 04 Task 1 lands the per-interface refresh: `doc.go`, `errors.go`, `tenant_service.go`, `kms.go`, `phone_hasher.go`, `settings.go`, `module.go`, `events.go`, plus `types_test.go` (TDD-first). Adds `Tenant.Validate()`, `TenantStatus.Valid()`, `Module`/`Deps`/`KMSClient` types, and `api.Register` seam. SettingsCache uses Lookup/LookupWithDefault/LookupAll (renamed from Get* to avoid the Tenancy-aggregate method-name collision with TenantService.Get). depguard now also blocks `internal/tenancy/{events,transport}`.
- **`internal/healthz/`** ✅ `Liveness`/`Readiness` handlers + `Checker` interface + `PostgresCheck`/`RedisCheck`/`NATSCheck`
- **`internal/modules/`** ✅ `Module` interface + `Deps` struct + `MapLocator` + `Registry`
- Per-module `internal/<X>/module.go` ✅ stubs with `Register(d modules.Deps) error { return nil }` — all 12 modules

### `migrations/`
- `000001_init.up.sql` / `.down.sql` — 19 tables, 19 RLS policies, `tenancy_admin BYPASSRLS` role, `app` user
- `000002_outbox.up.sql` / `.down.sql` — `event_outbox` table, indexes, owner=tenancy_admin

### `docs/`
- `architecture/00-overview.md` through `08-tdd-discipline.md` (8 design docs)
- `adr/0001-...md` through `0015-...md` (15 ADRs + README index)
- `superpowers/specs/2026-05-06-sociopulse-system-design.md` (~2700 lines spec)
- `superpowers/plans/2026-05-06-NN-...md` (22 implementation plans, 21 unfinished beyond 00/00a/00b/02/03)

---

## Next plans (in dependency order)

### Plan 04 — Tenancy Module 🎯 **NEXT**
The trunk module — almost every other module depends on `tenancy/api`. Core surfaces:
- `TenantService` — CRUD + status (active/suspended/archived)
- `KMSResolver` — per-tenant KEK from Yandex KMS (production) or local-keychain (dev)
- `SettingsCache` — typed key-value with hot-reload via NATS event
- `BucketProvisioner` — creates per-tenant S3 bucket
- `PhoneHasher` resolver — pulls per-tenant pepper, wraps `pkg/encryption.PhoneHasher`
- HTTP admin endpoints for Service-Owner (cross-tenant)
- gRPC mTLS service for sidecar auth
- NATS publishers for `tenant.<t>.tenancy.*` events

**Plan**: `docs/superpowers/plans/2026-05-06-04-tenancy-module.md` (~2200 lines, ~10-12 tasks).

### Plan 05 — Auth Module
Argon2id password hashing, JWT (HS256, 15min access, 30day refresh, refresh-rotation reuse detection), TOTP enroll/verify, RBAC matrix. **Depends on Plan 04** (TenantService for `Resolve` lookups).

### Plan 06 — CRM Module
Projects, respondents (PII envelope-encrypted via pkg/encryption), DNC list, quotas, batch import. **Depends on Plan 04**.

### Plan 07 — Surveys Module
Survey schema + DSL evaluator + WASM runtime (TinyGo per ADR-0008). **Depends on Plan 04**.

### Plans 08-14
- Plan 08: FreeSWITCH cluster (infra + Ansible)
- Plan 09: telephony-bridge sidecar (ESL + Router + Backpressure)
- Plan 10: dialer (OperatorFSM + CallQueue + RDD)
- Plan 11: realtime (WebSocket Hub + ListenIn)
- Plan 12: recording (gRPC Commit + S3 streaming + retention)
- Plan 13: analytics + reports (ClickHouse ingest + presets)
- Plan 14: billing (cost calc + tariffs + monthly margin)

### Plan 01 — Infrastructure (parallel track)
Yandex Cloud Terraform + Packer + Ansible + Helm. Plan 03 Task 1 (PgBouncer Helm chart) was deferred here.

### Plans 15-19 — Frontend
React + TypeScript work in `sociopulse-web` repo (NOT here).

### Plan 20 — Observability foundation (Prometheus/Grafana/Loki/Alertmanager)

---

## Standing rules / patterns

### TDD is mandatory (ADR-0015)
Every new function/method has a failing test FIRST. Watch it fail. Then minimal impl. See `docs/architecture/08-tdd-discipline.md` for Red-Green-Refactor canon.

### Skill discipline (samber/cc-skills-golang)
Loaded at `~/.agents/skills/golang-*/SKILL.md`. The 12 skills:
- `golang-concurrency` (BP1-BP9: goroutine exit, errgroup, NewTimer+Reset, goleak)
- `golang-context` (ctx first param, WithoutCancel for outlive-parent work)
- `golang-data-structures`
- `golang-design-patterns`
- `golang-error-handling` (`%w` wrapping, sentinels, single-handling rule, low-cardinality strings)
- `golang-grpc`
- `golang-modernize` (any over interface{}, range over int, slices/maps packages)
- `golang-safety` (comma-ok, no defer in loops, bounded conversions)
- `golang-security` (`crypto/rand`, AES-GCM, HMAC, parameterized SQL, ConstantTimeCompare)
- `golang-structs-interfaces` (small interfaces, defined where consumed, accept iface return struct)
- `golang-testing` (table-driven + t.Parallel, integration build tag, testify as helpers)
- `golang-troubleshooting`

### Subagent-driven development (superpowers)
- Fresh implementer subagent per task (model: opus)
- Two-stage review: spec compliance, then code quality
- Reviewer dispatches independent — never trust the implementer's report blindly

### Path adaptation in older plans
Several plans say `internal/<X>` for things that ended up in `pkg/<X>` after Plan 00a:
- Plan 02 says `internal/config/` → actual is `pkg/config/`
- Plan 02 says `internal/observability/` → actual is `pkg/observability/`
- `internal/healthz/` is correct as-is (project-specific)

When dispatching subagents, ALWAYS provide the path-correction note.

### gopls cache pollution
After subagent dispatches, gopls often shows stale errors (e.g., "undefined: X" when X is freshly defined). Always re-verify with direct `go build ./... && go vet ./... && go test ...`. If those are green, the diagnostic is noise.

### Commit hygiene
- Every subagent must commit at the end of its task. The user has had to remind me twice when subagents left work uncommitted.
- Tag at end of each plan: `v0.0.N-<plan-slug>`.
- Push to origin/main, watch CI to green before tagging.

### Docker Hub
- Image: `maxtakuro/sociopulse-api`
- Auto-push on every `main` commit via CI Docker job
- Public registry — no login to pull

### CI / CodeQL note
The CodeQL workflow itself runs successfully but Code Scanning isn't enabled in repo Settings → Code security. Failures are config-only, not code-level. Either enable Code Scanning in GitHub UI or accept CodeQL job failure as unrelated to code health.

---

## Repo URL & identity

- Repo: https://github.com/TakuroBreath/sociopulse-platform
- Local git config: `TakuroBreath / maxsmurffy@gmail.com` (set via `git config --local`)
- `gh` CLI is authenticated as `TakuroBreath`
- Docker Hub repo: https://hub.docker.com/r/maxtakuro/sociopulse-api (PAT in GitHub Secrets `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN`)

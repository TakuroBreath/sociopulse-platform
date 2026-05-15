# Plan 21 — E2E Smoke Foundation (tests/smoke + cmd/api module wiring)

> **Subagents must read this file BEFORE writing code.** It captures the
> canonical specs, the verified state of the codebase, and the gotchas
> that smoke-test scaffolding ALWAYS hits the first time. The plan file
> at `docs/superpowers/plans/2026-05-15-21-e2e-smoke-foundation.md`
> tells you WHAT to write; this file tells you WHERE the rakes are.

## 1. Canonical specs

- **Closure plan (the master design):** [`docs/architecture/10-end-to-end-testing-gaps.md`](../architecture/10-end-to-end-testing-gaps.md)
  - § "Phase 1 — `tests/smoke/`" lists 8 scenarios with priority order. This plan delivers the **first four** (Health/Auth/RBAC/TenantIsolation). The remaining four (project import, operator WS, surveys, recording-stream, 152-FZ purge) are explicitly out of scope and stay in the closure plan as future iterations.
  - § "Why this matters — concrete failure scenarios" #1–6 — the gap classes Phase 1 closes. Scenario #1 (locator wiring mismatch) is the canonical motivation for the `tenancy/auth/crm/surveys` wiring tasks at the start of this plan.
- **Testing strategy:** [`docs/architecture/04-testing-strategy.md`](../architecture/04-testing-strategy.md)
  - § "Layer 2 — Integration" — testcontainers-go patterns the smoke layer reuses. Container per test, not shared. `t.Cleanup(func() { _ = pgC.Terminate(ctx) })`.
  - § "What this strategy does NOT yet cover" — explicitly names `tests/smoke/` as the open gap closed by this plan.
- **Workflow improvements:** [`docs/architecture/09-agent-workflow-improvements.md`](../architecture/09-agent-workflow-improvements.md)
  - § Improvement #5 — original sketch for `tests/smoke/`; superseded by `10-end-to-end-testing-gaps.md` (more detail there).
- **TDD discipline:** [`docs/architecture/08-tdd-discipline.md`](../architecture/08-tdd-discipline.md) + ADR-0015.
  - Smoke-test TDD: Red = write the scenario, watch it fail because the missing wiring / endpoint / route is genuinely absent; Green = wire it; Refactor = extract helpers (`smokeStack`, `bootAPI`, etc.).
- **System design spec:** `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`
  - §17 — testing pyramid; §17.1 — load/chaos categories (post-Phase 1 scope).
  - §NFR-1 — perf budgets (NOT exercised by smoke, but mentioned in Phase 5 follow-ups).
- **ADRs:**
  - ADR-0006 (PgBouncer txn mode) — every request = one Tx; smoke tests bypass PgBouncer and talk straight to Postgres (testcontainer doesn't run PgBouncer in Phase 1).
  - ADR-0010 (NATS JetStream) — durable subjects. The smoke stack provisions wildcard streams BEFORE booting cmd/api (mirrors `cmd/api/main_test.go::ensureTestStream`).
  - ADR-0012 (zap), ADR-0014 (gin), ADR-0015 (TDD mandatory).
- **Domain glossary:** [`CONTEXT.md`](../../CONTEXT.md) — vocabulary canon. Test names use glossary terms (`Tenant`, `Operator`, `Respondent`, `FSM`, `RLS`, ...).

## 2. Reality-checked codebase state (verified 2026-05-15)

### 2.1 What's already wired in cmd/api (`cmd/api/main.go:339-346`)

```go
providers := modules.Registry{Modules: []modules.Module{
    telephony.Module{},
    dialerModule,
    recordingModule,
    analytics.New(analytics.Config{Registerer: metrics.Registry}),
    reports.New(reports.Config{ObjectStore: recordingObjects}),
    billing.Module{},
}}
```
Verified by: `cmd/api/main.go:339-346`.

**Missing:** `tenancy`, `auth`, `crm`, `surveys`. All four module.go files exist and are functional (Plans 04, 05, 06, 07 all shipped). The wiring just wasn't carried forward into the cmd/api registry.

Plan 14 Production Lesson #1 already flagged the `auth` gap:
> "**auth module needs to be in cmd/api/main.go providers registry.**"
Verified by: `docs/references/plan-14-billing.md:550`.

### 2.2 Module dependency order (locator entries)

| Module | Registers (locator key) | Consumes (locator key, source module) |
|---|---|---|
| `tenancy` | `tenancy.TenantService`, `tenancy.KMSResolver`, `tenancy.PhoneHasher`, `tenancy.Tenancy` | (none — bottom of stack) |
| `auth` | `auth.Authenticator`, `auth.UserService`, `auth.TOTPService`, `auth.RBACChecker`, `auth.ClaimsValidator`, `auth.SessionRevoker` | `tenancy.TenantService`, `tenancy.KMSResolver`, optional `audit.Logger` |
| `crm` | `crm.ProjectService`, `crm.RespondentService` | optional `tenancy.KMSResolver`, `tenancy.PhoneHasher`, `auth.RBACChecker`, `auth.ClaimsValidator`, `audit.Logger` |
| `surveys` | `surveys.SurveyService` | optional `auth.ClaimsValidator`, `auth.RBACChecker`, `audit.Logger` |

Verified by: `internal/{tenancy,auth,crm,surveys}/module.go` `requireDeps` + `Locator*` constants.

**Strict ordering:** `tenancy → auth → {crm, surveys}`. `tenancy` MUST come before any consumer in the providers walk because its `Register` is the only path that publishes `tenancy.TenantService` / `tenancy.KMSResolver` / `tenancy.PhoneHasher` into the locator.

**Insertion point in cmd/api/main.go:** all four go into the `providers` slice (BEFORE `consumers` which is realtime-only). `tenancy.Module{}` is FIRST; `auth/crm/surveys` follow the existing `recording → analytics → reports → billing` block (auth must precede crm + surveys; crm + surveys are siblings).

### 2.3 `tenancy.Module` has an init-time seam (gotcha)

`internal/tenancy/module.go::Register` returns nil-no-op IF `internal/tenancy/api.Register` is nil. The slot is filled by an `init()` in `internal/tenancy/service/register.go`. cmd/api **must blank-import the service package** to trigger the seam:

```go
_ "github.com/sociopulse/platform/internal/tenancy/service" // wire api.Register seam
```
Verified by: `internal/tenancy/module.go:50-52` + `internal/tenancy/api/register.go`.

Without the blank import the module reports "registered" but publishes NO locator entries, and `auth.Module.Register` will fail with `auth: tenancy.TenantService not registered`.

### 2.4 cmd/api/run() composition root is ALREADY callable from tests

`cmd/api/main.go:105` — `func run(ctx context.Context, configDir string) error`. The existing `cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly` uses this seam. **The smoke harness must reuse it as-is** — there is no need to refactor `Run(...) error` into a separate package. Plan 21 builds on top of the existing seam.

Pre-built helpers in `cmd/api/main_test.go` that smoke tests CAN reuse via a shared internal `tests/smoke/internal/...` (or via copying into the smoke package — both acceptable):
- `pickFreeAddr(t)` — ephemeral 127.0.0.1:N
- `listenerReadyChan(addr, timeout)` — non-polling readiness signal
- `ensureTestStream(t, name, subjects)` — JetStream stream provisioning, skip-on-no-NATS
- `writeMinimalDevConfig(t, httpBind, metricsBind)` — minimal YAML config writer

**Recommended pattern:** smoke tests write their own config with REAL DSNs (testcontainer DSNs) and reuse `pickFreeAddr` / `listenerReadyChan` directly. The existing `writeMinimalDevConfig` is for the unit-level boot test — its DSNs point at localhost, which a testcontainer-driven smoke run won't match.

### 2.5 Migration runner (`cmd/migrator`)

```
cmd/migrator/main.go --target={postgres,clickhouse}  # apply migrations
```

The smoke setup applies Postgres migrations via `cmd/migrator` against the testcontainer PG. ClickHouse migrations are NOT needed for Phase 1 scenarios (no analytics readback in scope).

**Gotcha:** `cmd/migrator` is a separate binary. The smoke setup options are:
1. `os/exec` the migrator binary (requires `go build ./cmd/migrator/...` step in CI).
2. Import the migrator's Go entry function and call it inline.

**Recommended:** option 2. Look at how `cmd/migrator` exposes its run function (likely already callable, mirroring cmd/api's `run()` seam). If not, refactor to a `Run(ctx, opts)` function as part of Task 4.

Verified by: existing `cmd/migrator/main.go` (Plan 13.1 added `--target=clickhouse` extension — so the file already has a callable run-loop structure).

### 2.6 testcontainers-go usage (project canon)

The project already uses testcontainers-go in `pkg/postgres`, `pkg/outbox`, `cmd/migrator` under `//go:build integration`. Phase 1 smoke layer ADDS a new build tag — `//go:build smoke` — that the smoke job in CI runs separately. The existing integration tag stays untouched.

Canonical patterns:
```go
//go:build smoke

package smoke_test  // not just `smoke` — keeps the package internal

import (
    "context"
    "testing"

    "github.com/stretchr/testify/require"
    "github.com/testcontainers/testcontainers-go/modules/postgres"
    "github.com/testcontainers/testcontainers-go/modules/redis"
)

func newSmokeStack(t *testing.T) *Stack { ... }
```

Container lifecycle:
- `t.Cleanup(func() { _ = container.Terminate(ctx) })` — never leak.
- 250 ms startup per Postgres container; the smoke suite has ONE stack per `TestMain` (or one per test depending on isolation needs — TBD by Task 4).

### 2.7 MinIO testcontainer for recording / reports

`tests/smoke/` does NOT need MinIO for the Phase-1 scenarios (Health/Auth/RBAC/TenantIsolation don't touch S3). But future scenarios (recording-stream — out of scope here) will, so the harness should be extensible to add MinIO without a rewrite. **Recommended:** make `smokeStack.S3` a nullable accessor — instantiated only when a test requests it. Do NOT pre-instantiate.

### 2.8 The `POST /api/calls/:id/hangup` open finding

Verified by: `internal/dialer/transport/http/routes.go:117` — `calls.POST("/:id/hangup", h.hangup)` with NO `RequireSameTenant` middleware in the chain.

Source of the finding: Plan 13.2.5 cross-tenant guard audit, recorded as:
> "Out-of-scope CRITICAL-class finding discovered during Task 1 review: `POST /api/calls/:id/hangup` lacks tenant scope — tracked for Plan 14."

Plan 14 then deferred it to v0.0.26. This plan (v0.0.26) closes it. The fix mirrors the Plan 13.2.5 pattern: `RequireSameTenant(resolveFn)` where `resolveFn(ctx, callID) → (uuid.UUID, error)`. The Router likely already has a `LookupCall(callID) → (Call, error)` path; if not, the call's tenant_id comes from `calls.tenant_id` via a BypassRLS read.

**The PR-time smoke `TestSmoke_TenantIsolation` covers this exact path** as a regression net.

### 2.9 JetStream stream pre-provisioning

Plan 14 Production Lesson highlighted (`PROJECT_STATUS.md:342`):
> "`cmd/api/main_test.go::ensureTestStream` provisions a `tenant.>` + `trunks.>` JetStream stream pair BEFORE `run()` starts (when NATS is reachable)."

The smoke harness MUST provision these streams BEFORE booting cmd/api. The dialer + realtime dispatcher + analytics subscriber + billing subscriber + recording outbox all assume the streams exist. A missing stream → boot fails with "no stream matches subject" → smoke gives a misleading error.

Re-use `ensureTestStream(t, "TENANT_SMOKE", []string{"tenant.>"})` + `ensureTestStream(t, "TRUNKS_SMOKE", []string{"trunks.>"})`. The streams use `InterestPolicy` + `MemoryStorage` so they evaporate at container teardown.

## 3. Library / SDK references (use `context7` to verify current APIs)

| Library | Used for | context7 ID |
|---|---|---|
| `github.com/testcontainers/testcontainers-go` | Container lifecycle | `/testcontainers/testcontainers-go` |
| `github.com/testcontainers/testcontainers-go/modules/postgres` | Postgres testcontainer | (same module path) |
| `github.com/testcontainers/testcontainers-go/modules/redis` | Redis testcontainer | (same) |
| `github.com/testcontainers/testcontainers-go/modules/nats` | NATS testcontainer | (same, look up exact subpath) |
| `github.com/testcontainers/testcontainers-go/modules/minio` | MinIO testcontainer (future use) | (same) |
| `github.com/nats-io/nats.go` | JetStream stream provisioning | already in `go.mod` |
| `github.com/stretchr/testify` | `require`/`assert` | already in `go.mod` |
| `coder/websocket` (`github.com/coder/websocket`) | WS dial (future scenario 3) | already in `go.mod` |

**Rule:** before writing code that calls a method on any of these libraries, run `mcp__plugin_context7_context7__resolve-library-id` then `query-docs` to confirm the current signature. testcontainers-go has had API churn in 2025 (`Run` vs `RunContainer`); don't guess.

## 4. Gotchas (known traps)

### 4.1 testcontainers-go reaper compatibility

testcontainers-go's "ryuk" reaper container has had Docker-version compatibility issues on macOS. If smoke tests hang at startup, set `TESTCONTAINERS_RYUK_DISABLED=true` in the CI env. Document the trade-off (leaked containers on test panic) in the smoke README.

### 4.2 cmd/api binds on `cfg.HTTP.Bind` AND `cfg.Observability.Metrics.Bind`

Both must be ephemeral 127.0.0.1:N for parallel test runs. `pickFreeAddr` returns immediately-closed ports — there's a tiny race where another process grabs the port between `Close()` and `run()`'s rebind. The existing test tolerates this; if smoke flakes on port-bind, switch to "let cmd/api pick :0 and then read back the bound address" (requires a small `run()` change). Document either way.

### 4.3 zap logger captures full goroutine stack traces on error

When a smoke test fails it's tempting to grep the logger output for the error. zap emits at INFO/WARN by default; configure `LogLevel: "debug"` in the smoke config and call `t.Log(string(logBytes))` on failure. Or wire the logger to write to `testing.T` directly (`zap.NewTee(writeSyncer(t), ...)`).

### 4.4 `go test -tags=smoke ./tests/smoke/...` from the repo root, not the package dir

The repo uses Go modules. Run all smoke tests via:
```bash
go test -tags=smoke -race -count=1 -timeout=10m ./tests/smoke/...
```
The `-timeout=10m` is conservative (testcontainer pulls can take 90 s on a cold Docker daemon).

### 4.5 The auth JWT secret is loaded from config

`auth.Module.Register` requires `Config.Auth.JWT.Secret` non-empty (`auth/module.go:241`). The smoke config writer MUST set a deterministic secret (e.g. `"smoke-jwt-secret-do-not-use-in-prod"`). Without it, auth Register returns `auth: Config.Auth.JWT.Secret is required (load from Lockbox)` and cmd/api logs WARN + skips.

### 4.6 KMS provider must be "local" in smoke

`tenancy.Module` config picks between Yandex SDK (build-tag gated, won't compile without `-tags=yandex_kms`) and the in-process `LocalKMSClient`. Smoke config MUST set `kms.provider: local` (or leave empty — empty defaults to local). Same for S3: `s3.provider: local`.

### 4.7 Migration ordering: PG migrations BEFORE cmd/api boots

cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly tolerates a missing PG (boots without Postgres connectivity, just warns). Smoke tests DON'T tolerate it — they need real CRM tables, real `tenants` row, real `users` row. Sequence:

1. Boot PG testcontainer
2. Apply migrations via cmd/migrator
3. Insert seed rows (tenant + admin user + operator user) via direct SQL or a helper
4. Boot cmd/api against the same DSN

Seeding requires knowing the password hash format. `internal/auth/service/authenticator.go` uses argon2id; the helper at `pkg/passwords.Default().Hash(password)` produces the PHC string. Use it.

### 4.8 Cross-tenant scope is enforced AT THE HANDLER, not at the FSM

The Plan 13.2.5 pattern: middleware reads `:id` from URL → `resolveFn` BypassRLS lookup → compare against `auth.ClaimsFromContext(c).TenantID` → 404-no-body on mismatch. For `:id/hangup`, the resolver is "look up `calls.tenant_id WHERE id = :id`" — likely via a new method on `dialer.Router` or a thin `internal/dialer/api.CallResolver` port.

If `dialer.CallResolver` already exists (Plan 11.4 wired `realtime.CallResolver`; the dialer side may share or duplicate), reuse it. Otherwise add the smallest reasonable port. **DO NOT** invent a new shape; check both `internal/dialer/api/` and `internal/realtime/service/cached_call_resolver.go` first.

## 5. Open questions (resolve before merging the plan)

1. **Should the smoke stack be ONE shared per-`TestMain`, or one per test?** Spec §17 says "containers run per test, not shared" for integration. For smoke, the cost (4 containers × 250 ms × 4 tests = 4 s overhead) is small enough that per-test isolation is preferred. Final answer: **per-`TestMain` shared stack with per-test cleanup of DB rows** (or per-test Schema namespacing). Trade-off documented in the harness's README.
2. **Do we need a `tests/smoke/internal/harness/` package?** Yes if multiple smoke files need the helpers. Implementer's call. Mirror the `tests/e2e/` layout convention (currently empty — Plan 21 sets the precedent).
3. **What's the cmd/api binary doing in CI smoke?** It runs as a goroutine in the same process as the test, NOT as a separate binary. This matches `cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly` and avoids the `os/exec` complexity of attaching to a child process.
4. **Should TestSmoke_TenantIsolation also exercise `:id/hangup`?** Yes — that's the regression net for the Task 3 fix. The scenario tests at minimum: GET `/api/projects/:id` (covered by Plan 13.2.5), POST `/api/calls/:id/hangup` (new, this plan).

## 6. Production lessons (post-execution 2026-05-15)

### Architecture / wiring

1. **`tenancy.Module.Register` is nil-no-op without the blank-import.** `internal/tenancy/module.go:50-52` checks `if api.Register == nil { return nil }`. The slot is filled by `init()` in `internal/tenancy/service/register.go`. cmd/api MUST blank-import the service package (`_ "github.com/sociopulse/platform/internal/tenancy/service"`); without it the module reports `"registered"` in logs but publishes zero locator entries, and auth/crm/surveys downstream consumers fail with `auth: tenancy.TenantService not registered`. The unit-test `TestBuildProviders_TenancyIsFirstEntry` pins both presence AND the blank-import effect (`require.NotNil(t, tenancyapi.Register, ...)`).

2. **`auth.Module.Register` requires `Config.Auth.JWT.Secret` non-empty + `mapstructure` binding active.** The original Plan 04 / 05 design left `JWTConfig.Secret` with `mapstructure:"-"` because "production loads from Lockbox at runtime". In practice, this blocked auth.Register from EVER succeeding in any in-process boot path (dev / smoke / unit-test) because the field had no other write path. Plan 21 Task 6 removed the tag; production hygiene is enforced by `cfg.Validate()` (config.go:96 requires `SecretLockboxKey` when env=production). The env var `SOCIOPULSE_AUTH_JWT_SECRET` is the canonical prod path via Kubernetes Secret → viper AutomaticEnv → `pkg/config`. This unblocks Plan 14's open follow-up #1 ("auth not in cmd/api registry").

3. **gin's URL wildcard names must agree across modules sharing a path-position.** dialer mounted `/api/calls/:id/{status,hangup}` (Plan 10); recording mounted `/api/calls/:call_id/recording{,/verify}` (Plan 12.3) — both into the same gin engine. When auth WARN-skipped (Plan 14 era), some module-ordering edge cases masked the conflict; Plan 21 Task 2 stabilised the providers walk, and the full registration triggered `panic: ':call_id' conflicts with existing wildcard ':id'`. Fix: rename URL wildcards to a project-wide convention (`:id` won). The structured-log field names + JSON tags (`json:"call_id"`) are INDEPENDENT of URL routing — those stayed `call_id` because they're wire/storage contracts.

4. **asynq's `*FromRedisClient` constructors are mandatory when multiple asynq components share a Redis client.** `crm.Module.Register` originally used `asynq.NewServer/NewClient/NewScheduler` with `RedisConnOpt`. Each component then independently calls `broker.Close()` (which closes the underlying client) at Shutdown. With the providers walk activating multiple asynq users in the same cmd/api process, the first Shutdown closes the client, and sibling subscribers' next pubsub read panics on a nil message (`subscriber.go:83` in asynq@v0.26.0). Plan 21 Task 2 switched all three constructors to `*FromRedisClient` (sharedConnection=true), and `Stop()` now drops the client reference rather than calling `Close` (which would return a non-fatal but useless "redis connection is shared" error).

5. **`tenancy_admin` SELECT grants on tenant-scoped tables are required for any new `BypassRLS` read path.** Plan 12.4 added grant migration `000011` for `call_recordings`; Plan 21 Task 3 added `000014` for `calls`. Without the grant, `pool.BypassRLS` (which `SET LOCAL ROLE tenancy_admin`) fails with `42501 permission_denied`. Pattern: any new `*Resolver` adapter that uses `BypassRLS` requires a parallel grant migration. Writes stay under `pool.WithTenant`, so no INSERT/UPDATE grants on the same migration — read-only.

### Testing harness

6. **`TESTCONTAINERS_RYUK_DISABLED=true` is mandatory on macOS Docker.** testcontainers-go's "ryuk" reaper container spawns a goroutine `(*Reaper).connect.func1` in `reaper.go:535` that does NOT terminate within `goleak.VerifyTestMain`'s window. `tests/smoke/stack.go::init()` sets the env var before any container starts; the CI job sets it at the job level too. Trade-off: a test panic mid-run leaves orphan containers; clean with `docker ps -a --filter label=org.testcontainers=true -q | xargs docker rm -f`. The references file flagged this in § 4.1 before execution; the implementer's first Task 4 commit (`76a77a8`) shipped a README mention but didn't APPLY the workaround — the test was red until `fbe9fad` fixup. Lesson: an env-var workaround documented in README is not the same as an env-var workaround applied in code.

7. **golang-migrate inline is simpler than refactoring cmd/migrator.** The plan offered three migration-runner options for the smoke harness: (i) os/exec the binary, (ii) refactor cmd/migrator's main() to expose a Go entry, (iii) call golang-migrate/migrate/v4 directly. Option (iii) won because golang-migrate was already in go.mod; the harness imports `"github.com/golang-migrate/migrate/v4"` + the postgres+file driver blank-imports, then `migrate.New("file://"+repoRoot+"/migrations", dsn).Up()` against the testcontainer DSN. Zero cmd/migrator changes. ~10 LoC.

8. **Per-`TestMain` shared stack is the right granularity for smoke.** One PG + Redis + NATS set, all smoke tests in the package reuse. Per-test isolation via `t.Cleanup` row-deletion in seed helpers. Cold container pulls ~90 s (first test); warm steady-state ~6–9 s per scenario. Per-test container teardown would multiply the cost 5× without a real benefit (the harness is fast enough that scenarios serialise the boot via `sync.Once` on the shared stack initialiser).

9. **`cmd/api/main.run()` stays unexported.** The plan offered two paths for exposing the composition root to smoke: (a) extract `run` into a public `cmd/api/internal/runner` package, or (b) place smoke tests INSIDE `cmd/api/` so they can call `run()` directly. (b) won — the extract would cascade ~1700 LoC across 12 helper files (postgres.go / redis.go / eventbus.go / server.go / providers.go / modules.go / realtime.go / recording.go / recording_resolver.go all transitively reference `run()`'s locals). Path-of-least-surface: reusable harness LIBRARY in `tests/smoke/` (Stack, helpers, seeds), one smoke TEST file `cmd/api/smoke_test.go` that calls `run()`. The pattern matches the pre-existing `cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly`.

10. **JetStream wildcard streams must be pre-provisioned BEFORE cmd/api boot.** `cmd/api/main.go:485` — the realtime dispatcher's subscriber treats "no stream matches subject" as a hard error. The smoke harness mirrors `cmd/api/main_test.go::ensureTestStream`: connect to NATS, `AddStream(InterestPolicy + MemoryStorage)` for `tenant.>` and `trunks.>`. Memory storage so containers don't accumulate state. Pre-provisioning is in `bootAPI` (not `newSmokeStack`) so each test re-arms streams in case a previous scenario consumed messages.

### Auth / RBAC

11. **The login DTO uses `org_id` not `org_code` — verified, not assumed.** `internal/auth/transport/http/dto.go:23` — `OrgID string \`json:"org_id" binding:"required,min=1,max=64"\``. The plan's example used `org_code` (matching the DB column `tenants.org_code`); the wire path uses `org_id`. The plan's references file should have caught this; future plans must verify both DB column AND HTTP DTO when writing scenarios. (`SeededAccount` carries `OrgCode` semantically — the value passed to login is the same string regardless of the field-name spelling on either side.)

12. **`users.roles` is `text[]` not `text`** (per `migrations/000005_auth.up.sql` — verify before seeding). The original Plan 05 design had `role text` single-valued; Plan 13.2.5 expanded to multi-role via `roles []string` in JWT claims. The seed `INSERT INTO users ... VALUES (..., ARRAY['admin']::text[], ...)` for admin and `ARRAY['operator']::text[]` for operator.

13. **`projects.customer` reads as `string` not `*string` in `ProjectStore.scanRow`.** A NULL value in this column makes `pgx.Scan` fail, and the BypassRLS resolver in `RequireSameTenant` returns a generic error which the middleware downgrades to **500** (not 404). The seed MUST set `customer=''` (empty string) explicitly. This is a latent bug in `crm/store/project_store.go` (the type should be `*string` or `sql.NullString`) — out of scope for Plan 21, but the seed comment flags it for future cleanup.

### Operational

14. **POST /api/calls/:id/hangup cross-tenant smoke regression net.** The Plan 13.2.5 audit found this gap and deferred to "Plan 14". Plan 21 actually closed it with `RequireSameTenant` + new `CallTenantResolver` port. The smoke-level regression net is `TestSmoke_TenantIsolation` which exercises both the existing `GET /api/projects/:id` cross-tenant path (RLS + middleware) AND the new `POST /api/calls/:id/hangup` path. Future cross-tenant fixes should follow the same pattern: middleware FRONT (`RequireSameTenant(resolveFn)`) + explicit-`callerTenantID`-param BACK + smoke regression test.

15. **CI smoke job runtime ~3–5 min on cold cache, ~1 min on warm.** Pre-pull `postgres:16 / redis:7 / nats:2.10` in the workflow step before `make test-smoke` to keep first-pull tail latency out of the timeout budget. 20-minute job timeout is conservative; in practice the run completes in 2–3 minutes warm. The `build` job's `needs: [lint, test, smoke]` gates v* tag pushes on smoke green, so a flaky smoke is a production block — the per-`TestMain` shared-stack pattern is critical to keep flakes near-zero (per-test fresh containers would 5× the duration AND multiply Docker daemon races by 5).

### Phase-1b (deferred, captured here so future agents know what's left)

16. **The remaining 4 Phase-1 scenarios** (project import / operator WS / surveys CRUD / recording stream / 152-FZ purge) need:
    - MinIO testcontainer (recording stream, project import async result)
    - asynq Worker harness for cmd/worker integration (project import, 152-FZ purge)
    - `coder/websocket` client wrapper (operator WS pipeline)
    - Injectable clock (152-FZ purge — fast-forward 30 days)
    - Survey schema fixtures (surveys CRUD)
    Each is a logical Phase-1b task. The Plan 21 harness shape extends naturally; no rewrites needed.

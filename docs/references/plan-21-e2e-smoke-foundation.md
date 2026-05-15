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

## 6. Production lessons (post-execution YYYY-MM-DD — fill at close-out)

<!-- Filled at close-out per CLAUDE.md rule #8. Examples of what to capture:
- Container startup timing on CI vs local
- Any module-wiring gotchas that surfaced during Task 1/2
- Any test flakes and what fixed them (port-bind races, JetStream stream lag, etc.)
- testcontainers-go API quirks discovered via context7
- Whether the per-`TestMain` shared-stack decision held up
-->

(Filled at close-out.)

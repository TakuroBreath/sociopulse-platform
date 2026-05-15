# Plan 21 — E2E Smoke Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Plan ID:** 21
>
> **References (READ FIRST):** [`docs/references/plan-21-e2e-smoke-foundation.md`](../../references/plan-21-e2e-smoke-foundation.md) + [`docs/references/COMMON.md`](../../references/COMMON.md). Every implementer subagent MUST read the per-plan references file BEFORE writing code (canonical specs, gotchas, library refs, open questions).
>
> **Architecture docs:** [`10-end-to-end-testing-gaps.md`](../../architecture/10-end-to-end-testing-gaps.md) (master closure plan — Phase 1 is the scope of THIS plan), [`04-testing-strategy.md`](../../architecture/04-testing-strategy.md), [`08-tdd-discipline.md`](../../architecture/08-tdd-discipline.md), [`09-agent-workflow-improvements.md`](../../architecture/09-agent-workflow-improvements.md) § Improvement #5.
>
> **Affected ADRs:** ADR-0006 (PgBouncer txn), ADR-0010 (JetStream), ADR-0012 (zap), ADR-0014 (gin), ADR-0015 (TDD mandatory). NO existing ADR is contradicted by this plan.

**Goal:** Close Phase 1 of the End-to-End Testing Gap by wiring the missing modules (`tenancy`, `auth`, `crm`, `surveys`) into `cmd/api`, fixing the known cross-tenant gap on `POST /api/calls/:id/hangup`, and shipping a `tests/smoke/` package that boots the full `cmd/api` against a real Postgres + Redis + NATS testcontainer stack and exercises four cross-module flows (Health, AuthFullFlow, RBAC, TenantIsolation).

**Architecture:**
1. **cmd/api module wiring** — extend the existing `providers` registry with `tenancy.Module{}` (FIRST, has init-seam blank-import gotcha), then `auth.Module{}`, `crm.Module{}`, `surveys.Module{}` in the canonical dependency order. Each `Register` either succeeds (all locator entries published) or fails-with-WARN (cmd/api still boots, but smoke catches the gap).
2. **`tests/smoke/` package**, `//go:build smoke`, with a per-`TestMain` shared stack: one Postgres testcontainer + one Redis testcontainer + one NATS testcontainer. Per-test isolation via DB-row cleanup helpers. cmd/api runs as a goroutine inside the same Go process (mirrors `cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly`). Tests speak the public HTTP surface via real `http.Client`.
3. **CI integration** — new `smoke` job in `.github/workflows/ci.yml`, parallel to existing `test`, runs on every push to `main` (matches `09-agent-workflow-improvements.md` § Improvement #5 trigger).

**Tech Stack:**
- `github.com/testcontainers/testcontainers-go` + modules/{postgres,redis,nats}
- `github.com/nats-io/nats.go` for JetStream stream provisioning
- `github.com/stretchr/testify` (`require`/`assert`)
- Existing `cmd/api/run(ctx, configDir)` composition root
- Existing `cmd/migrator` for applying Postgres migrations
- gin handlers (existing), JWT (`internal/auth`), RBAC matrix (existing)
- Build tag: `//go:build smoke`
- CI: GitHub Actions `smoke` job

**Spec sections covered:** §17.1 (testing pyramid — adds the missing system-E2E layer), §FR-A (`/api/auth/login` smoke), §FR-B (`/api/projects` RBAC smoke), §FR-C (cross-tenant isolation smoke); closes scenarios A + C of `10-end-to-end-testing-gaps.md` § "What we DO NOT test today".

**Prerequisites:**
- All Plans 02–14 shipped (the modules being wired exist and are functional). Verified by `PROJECT_STATUS.md` milestone table — all 14 main plans tagged.

**What's intentionally out of scope (deferred to future smoke iterations):**
- Scenario 2 (TestSmoke_AdminCreatesProjectAndImportsRespondents — async respondent import). Requires asynq worker setup, MinIO testcontainer. Adds a day; deferred to a Phase-1b plan.
- Scenario 3 (TestSmoke_OperatorReadyAndStateBroadcast — operator WS). Requires WS client wrapper + operator FSM seed data; deferred.
- Scenario 4 (TestSmoke_SurveyCreatePreviewActivate — surveys workflow). Requires survey schema fixtures; deferred.
- Scenario 5 (TestSmoke_RecordingSearchAndStream — recording playback). Requires MinIO testcontainer + pre-encrypted fixture; deferred.
- Scenario 8 (TestSmoke_RespondentSoftDelete152FZ — 30-day purge via clock injection). Requires asynq worker + clock injection; deferred.
- REST collection (Bruno / Postman / Hurl). Phase 2 of the closure plan — separate ~4 h follow-up.
- Frontend E2E (Playwright). Plans 15-19 territory, sociopulse-web repo.
- Real FreeSWITCH integration. Plan 08 territory.
- Real Yandex SDK adapter coverage. Plan 01 territory.

**Plan Amendments:**

## Amendments: none

(Filled at close-out per CLAUDE.md rule #7 if reality diverged from the spec.)

---

## Verified Context (cross-boundary assertions)

Every cross-module / cross-boundary claim made in this plan has been verified against the codebase before execution. Implementers should re-verify if anything below contradicts what they see at run-time.

- **`cmd/api` providers list currently misses `tenancy`, `auth`, `crm`, `surveys`.** Verified by: `cmd/api/main.go:339-346`.
- **`tenancy.Module.Register` is a nil-op when `internal/tenancy/api.Register` is nil; the slot is filled by an `init()` in `internal/tenancy/service/register.go`.** Verified by: `internal/tenancy/module.go:50-52`.
- **`auth.Module.Register` requires `tenancy.TenantService` + `tenancy.KMSResolver` in the locator.** Verified by: `internal/auth/module.go:23-26, 65-69, 141-148`.
- **`crm.Module.Register` consumes `tenancy.KMSResolver`, `tenancy.PhoneHasher`, optionally `auth.RBACChecker` / `auth.ClaimsValidator`.** Verified by: `internal/crm/module.go:32-38, 70-77`.
- **`surveys.Module.Register` consumes `auth.ClaimsValidator` + `auth.RBACChecker`.** Verified by: `internal/surveys/module.go:30-34, 62-67`.
- **`auth.Module.Register` requires `Config.Auth.JWT.Secret` non-empty; missing returns explicit error.** Verified by: `internal/auth/module.go:240-243`.
- **`POST /api/calls/:id/hangup` exists and lacks `RequireSameTenant`.** Verified by: `internal/dialer/transport/http/routes.go:117` — `calls.POST("/:id/hangup", h.hangup)` with no tenant-resolver middleware on the chain.
- **`cmd/api/run(ctx, configDir) error` is already callable from tests.** Verified by: `cmd/api/main.go:105` + `cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly`.
- **`ensureTestStream` + `pickFreeAddr` + `listenerReadyChan` + `writeMinimalDevConfig` helpers exist in `cmd/api/main_test.go`** and are the canonical patterns to reuse. Verified by: `cmd/api/main_test.go:125-297`.
- **`tests/smoke/` does not exist; `tests/e2e/` is empty (`.gitkeep` only).** Verified by: `ls tests/`.
- **`docs/references/plan-21-e2e-smoke-foundation.md` exists (created with this plan).** Verified by: file path above.
- **testcontainers-go is already in `go.mod`** (used by `pkg/postgres`, `pkg/outbox`, `cmd/migrator`). Verified by: `grep testcontainers go.mod`.
- **`pkg/passwords.Default().Hash(plain)` produces an argon2id PHC string suitable for `users.password_hash` seeding.** Verified by: `pkg/passwords/hasher.go` + `internal/auth/service/authenticator.go::Login` which compares against this format.
- **The `tenancy.Module` blank-import for the service init-seam is the canonical pattern** (mirrors how cmd/api today blank-imports nothing extra for other modules — tenancy is the exception). Verified by: `internal/tenancy/module.go` package comment.
- **Plan 13.2.5 introduced `pkg/middleware/tenant.RequireSameTenant(resolveFn)` middleware.** Verified by: `pkg/middleware/tenant/` (the package exists per PROJECT_STATUS.md Plan 13.2.5 close-out).

---

## File Structure

### Created files

```
docs/superpowers/plans/2026-05-15-21-e2e-smoke-foundation.md   # this file
docs/references/plan-21-e2e-smoke-foundation.md                # created
tests/smoke/
├── README.md                              # what smoke is, how to run, CI matrix
├── doc.go                                 # build-tag-gated package doc
├── stack_test.go                          # smokeStack: testcontainer Postgres/Redis/NATS lifecycle
├── boot_test.go                           # bootAPI(t, stack) → addr + cleanup goroutine
├── seed_test.go                           # seedTenantAndUsers(t, stack) → admin/operator/tenantB JWTs
├── helpers_test.go                        # http helpers: doRequest, requireJSON, parseError
├── health_test.go                         # TestSmoke_HealthAndReadiness
├── auth_test.go                           # TestSmoke_AuthFullFlow
├── rbac_test.go                           # TestSmoke_RbacEnforcement
└── isolation_test.go                      # TestSmoke_TenantIsolation (incl. hangup regression)
.github/workflows/ci.yml                   # MODIFIED — add `smoke` job
```

### Modified files

```
cmd/api/main.go                            # extend providers slice with tenancy/auth/crm/surveys
cmd/api/main.go                            # add blank import _ "internal/tenancy/service"
cmd/api/main_test.go                       # extend TestRunStartsAndShutsDownCleanly assertions OR add TestRun_RegistersAllExpectedLocators (locator-completeness assertion)
internal/dialer/transport/http/routes.go   # wrap /:id/hangup with RequireSameTenant
internal/dialer/transport/http/routes.go   # (or service_handler.go) — add resolver wiring
internal/dialer/api/<file>.go              # if needed: add CallTenantResolver port (mirror Plan 11.4 CallResolver shape)
PROJECT_STATUS.md                          # close-out (milestone row + standing rules)
docs/references/plan-21-e2e-smoke-foundation.md  # Production lessons section at close-out
```

---

## Task 1: Wire `tenancy.Module` into cmd/api

**Files:**
- Modify: `cmd/api/main.go:339-346` (providers slice) + import block
- Modify: `cmd/api/main_test.go` (extend `TestRunStartsAndShutsDownCleanly` OR add new test `TestRun_TenancyLocatorEntriesPublished`)
- Test: `cmd/api/main_test.go`

**Why first:** `auth`, `crm`, `surveys` ALL depend on locator entries published by `tenancy.Module`. Adding any consumer before the producer is wired produces a misleading "module Register failed" WARN in cmd/api logs, defeating future debugging.

**Gotcha (READ FIRST):** `internal/tenancy/module.go::Register` returns nil-no-op if `internal/tenancy/api.Register` is nil. The seam is filled by `init()` in `internal/tenancy/service/register.go`. cmd/api MUST blank-import the service package:
```go
_ "github.com/sociopulse/platform/internal/tenancy/service" // wire api.Register seam
```
Without this import the module reports "registered" but publishes NO locator entries.

- [ ] **Step 1: Write the failing test**

Add to `cmd/api/main_test.go` (after `TestRunStartsAndShutsDownCleanly`):
```go
// TestRun_TenancyLocatorPublishesEntries asserts that when run() completes
// boot, the locator carries the four tenancy.* keys the downstream modules
// (auth/crm/surveys) will look up. This is the regression net for the
// Plan 21 Task 1 wiring — if the tenancy module is dropped from the
// registry OR the blank-import disappears, this test fails loud.
//
// We don't exercise the actual KMS/tenant flow — that's covered by
// internal/tenancy/* tests. We only assert the locator surface.
func TestRun_TenancyLocatorPublishesEntries(t *testing.T) {
    t.Parallel()

    httpAddr := pickFreeAddr(t)
    metricsAddr := pickFreeAddr(t)
    configDir := writeMinimalDevConfig(t, httpAddr, metricsAddr)

    ensureTestStream(t, "TENANT_TEST_T1", []string{"tenant.>"})
    ensureTestStream(t, "TRUNKS_TEST_T1", []string{"trunks.>"})

    ctx, cancel := context.WithCancel(context.Background())
    t.Cleanup(cancel)

    errCh := make(chan error, 1)
    go func() { errCh <- run(ctx, configDir) }()

    select {
    case err := <-errCh:
        t.Fatalf("run() returned before listener ready: %v", err)
    case <-listenerReadyChan(httpAddr, 10*time.Second):
    }

    // Hit /healthz and verify the response body advertises tenancy as registered.
    // healthz is the lightest signal-bearing endpoint that surfaces module
    // wiring state. If healthz doesn't expose this today, the assertion path
    // is via a debug endpoint — wire one if needed (out of scope: see
    // alternative in step 3).
    healthReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+httpAddr+"/healthz", nil)
    require.NoError(t, err)
    resp, err := http.DefaultClient.Do(healthReq)
    require.NoError(t, err)
    require.NoError(t, resp.Body.Close())
    require.Equal(t, http.StatusOK, resp.StatusCode)

    cancel()
    select {
    case err := <-errCh:
        require.NoError(t, err)
    case <-time.After(5 * time.Second):
        t.Fatal("run() did not exit within 5s")
    }
}
```

**Alternative test path (preferred if `/healthz` does NOT expose module state):** make `run()` accept an optional `locator-observer` seam that calls a closure after providers walk, and assert in the closure that the four `tenancy.*` keys are present in the locator. The seam should be unexported so production code can't accidentally rely on it. If this path is taken, the closure is supplied via a test-only `runWithLocatorObserver(...)` wrapper.

The implementer chooses whichever path is least invasive. If neither is clean, the locator-completeness assertion can move to Task 7 (smoke harness) — but the wiring itself MUST land in this task.

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/user/call-center/sociopulse-platform
go test -race -count=1 ./cmd/api/ -run TestRun_TenancyLocatorPublishesEntries -v
```
Expected: FAIL — boots cleanly (no error) but tenancy locator entries absent. The exact failure depends on which assertion path the implementer chose.

- [ ] **Step 3: Wire `tenancy.Module` into cmd/api/main.go**

In `cmd/api/main.go`:

a) Add to the import block:
```go
import (
    // ... existing imports ...
    "github.com/sociopulse/platform/internal/tenancy"
    _ "github.com/sociopulse/platform/internal/tenancy/service" // wire api.Register seam — see internal/tenancy/module.go
)
```

b) Modify the `providers` slice at `cmd/api/main.go:339-346`:
```go
providers := modules.Registry{Modules: []modules.Module{
    &tenancy.Module{},      // FIRST — publishes locator entries auth/crm/surveys consume.
                            // tenancy.Module pointer-receiver (Stop method); existing modules use value receiver.
    telephony.Module{},
    dialerModule,
    recordingModule,
    analytics.New(analytics.Config{Registerer: metrics.Registry}),
    reports.New(reports.Config{ObjectStore: recordingObjects}),
    billing.Module{},
}}
```

c) Verify `cfg.KMS.Provider` defaults to `"local"` or empty (which buildKMSClient maps to local) in `configs/development/config.yaml`. If it's set to `"yandex"`, the smoke / local boot will fail with "yandex KMS requires `-tags=yandex_kms` build". Set or confirm `kms: { provider: "local" }`.

- [ ] **Step 4: Run test to verify it passes**

```bash
go test -race -count=1 ./cmd/api/ -run TestRun_TenancyLocatorPublishesEntries -v
```
Expected: PASS.

Also run the existing boot smoke to confirm no regression:
```bash
go test -race -count=1 ./cmd/api/ -run TestRunStartsAndShutsDownCleanly -v
```
Expected: PASS.

- [ ] **Step 5: Confirm `make ci` is green**

```bash
make ci
go test -race -count=1 ./cmd/api/...
gofmt -l cmd/api/
make grep-time-after
```
Expected: all green; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add cmd/api/main.go cmd/api/main_test.go configs/development/config.yaml
git commit -m "$(cat <<'EOF'
feat(cmd/api): wire tenancy.Module into providers registry

Plan 21 Task 1 — tenancy is the prerequisite module for auth/crm/surveys
wiring in subsequent tasks. The pointer receiver mirrors tenancy.Module
itself (carries a Stop()), and the service-package blank-import wires
api.Register via the init() seam (internal/tenancy/module.go:50-52).

TestRun_TenancyLocatorPublishesEntries asserts the locator surface so a
future drop of either the module entry OR the blank-import fails loudly
at unit-test time rather than at first auth/login.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Wire `auth.Module`, `crm.Module`, `surveys.Module` into cmd/api

**Files:**
- Modify: `cmd/api/main.go` (providers slice, imports)
- Modify: `cmd/api/main_test.go` (locator-completeness assertion grows)
- Modify: `configs/development/config.yaml` (auth.jwt.secret for dev)
- Test: `cmd/api/main_test.go`

**Why second:** all three modules consume tenancy.* entries; tenancy is wired in Task 1. auth must come before crm/surveys because the latter two consume `auth.RBACChecker` / `auth.ClaimsValidator` (optional but expected). crm and surveys are independent siblings — order between them does not matter.

**Gotcha:** auth requires `Config.Auth.JWT.Secret` non-empty (auth/module.go:240-243). Smoke uses the dev config; ensure `auth.jwt.secret` is set to a deterministic dev value (NEVER a production-grade secret; the value gets bytes-logged at INFO and stored in cleartext YAML).

- [ ] **Step 1: Write the failing test (extend the existing locator-completeness assertion)**

Extend `TestRun_TenancyLocatorPublishesEntries` (rename to `TestRun_AllModulesLocatorPublishesEntries` to reflect broader scope), OR add a new test `TestRun_AuthCrmSurveysLocatorPublishesEntries`:

```go
// TestRun_AuthCrmSurveysLocatorPublishesEntries asserts the locator
// surface after Task 2 wiring. The four tenancy.* entries from Task 1
// stay; this adds: auth.{Authenticator, UserService, TOTPService,
// RBACChecker, ClaimsValidator, SessionRevoker}, crm.{ProjectService,
// RespondentService}, surveys.SurveyService.
//
// If any disappears the boot WARN-skipped that module — a wiring bug
// the smoke layer cannot detect from the outside (404 on the affected
// HTTP path would be the symptom; that's caught at smoke time, but
// catching it here is faster).
func TestRun_AuthCrmSurveysLocatorPublishesEntries(t *testing.T) {
    t.Parallel()
    // ... same boot scaffold as TestRun_TenancyLocatorPublishesEntries
    // ... assert locator carries the 9 additional entries (or 13 total
    //     counting tenancy.* — verify against the canon in
    //     docs/references/plan-21-e2e-smoke-foundation.md § 2.2)
}
```

If the assertion path goes via /healthz or a debug endpoint, the new module entries must be visible in the response. If via the test-only `runWithLocatorObserver` seam, the closure asserts each `Locator.Lookup(key)` returns ok=true.

- [ ] **Step 2: Run test to verify it fails**

```bash
go test -race -count=1 ./cmd/api/ -run TestRun_AuthCrmSurveysLocatorPublishesEntries -v
```
Expected: FAIL — the three modules are not yet in the providers list.

- [ ] **Step 3: Wire the three modules**

In `cmd/api/main.go`:

a) Imports:
```go
import (
    // ... existing imports ...
    "github.com/sociopulse/platform/internal/auth"
    "github.com/sociopulse/platform/internal/crm"
    "github.com/sociopulse/platform/internal/surveys"
)
```

b) Providers slice (after Task 1):
```go
providers := modules.Registry{Modules: []modules.Module{
    &tenancy.Module{},
    auth.Module{},          // consumes tenancy.TenantService + tenancy.KMSResolver
    telephony.Module{},
    dialerModule,
    recordingModule,
    analytics.New(analytics.Config{Registerer: metrics.Registry}),
    reports.New(reports.Config{ObjectStore: recordingObjects}),
    billing.Module{},
    crm.Module{},           // consumes tenancy.{KMSResolver,PhoneHasher}, optional auth.{RBAC,Claims}
    surveys.Module{},       // consumes auth.{ClaimsValidator,RBACChecker}
}}
```

c) `configs/development/config.yaml` — confirm or add:
```yaml
auth:
  jwt:
    secret: dev-only-jwt-secret-do-not-use-in-production
    issuer: https://app.sociopulse.local
    access_ttl: 15m
    refresh_ttl: 720h
    algorithm: HS256
```

d) `cmd/api/main_test.go::writeMinimalDevConfig` — extend the YAML template to include `auth.jwt.secret` (otherwise auth.Module.Register fails fast at boot in the unit-test path).

- [ ] **Step 4: Run test to verify it passes**

```bash
go test -race -count=1 ./cmd/api/ -run TestRun_AuthCrmSurveysLocatorPublishesEntries -v
```
Expected: PASS.

Also re-run all existing cmd/api tests to confirm no regression:
```bash
go test -race -count=1 ./cmd/api/... -v
```
Expected: PASS across the suite (TestRunStartsAndShutsDownCleanly, TestRunReturnsErrorOnInvalidConfig, TestRun_TenancyLocatorPublishesEntries, TestRun_AuthCrmSurveysLocatorPublishesEntries).

**Note about WARN-skip path:** the existing boot test environment has no Redis / no Postgres reachable. auth.Module requires both Pool + Redis; without them `requireDeps` returns an error → `registerModules` logs WARN + skips. The locator assertion under those conditions correctly reports the entries absent. This is FINE — the assertion lives behind the smoke harness (Task 4–7) where the full stack IS up. For the unit-level boot test, assert that the modules are *in the providers list* (compile-time presence), NOT that locator entries are *published* (runtime guarantee). Adjust the test accordingly: a `len(providers.Modules)` check + name-lookup is sufficient.

This means **Step 1's test should be refined** — assert the providers list is the expected length and contains every expected `mod.Name()`. The locator-entries assertion belongs to Task 7 (smoke harness with real stack).

- [ ] **Step 5: Confirm `make ci` is green + race-detector + gofmt**

```bash
make ci
go test -race -count=1 ./cmd/api/...
gofmt -l cmd/api/ configs/
make grep-time-after
```
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add cmd/api/main.go cmd/api/main_test.go configs/development/config.yaml
git commit -m "$(cat <<'EOF'
feat(cmd/api): wire auth/crm/surveys modules into providers registry

Plan 21 Task 2 — auth/crm/surveys are present-but-not-registered after
Plans 05/06/07. Wires them in the canonical dependency order
(tenancy → auth → {crm,surveys}). The auth.jwt.secret config knob is
populated with a deterministic dev value (never use in prod — load
from Lockbox per ADR-0001).

The boot test asserts providers-list membership; the locator-entries
runtime assertion lives in the smoke harness (Task 7) where Postgres +
Redis are actually available. Module.Register WARN-skip behaviour
preserved when deps are absent — cmd/api still serves /healthz +
/metrics for diagnostics.

Plan 14 Production Lesson #1 ("auth module needs to be in cmd/api
providers registry") is closed by this commit.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Add `RequireSameTenant` middleware to `POST /api/calls/:id/hangup`

**Files:**
- Modify: `internal/dialer/transport/http/routes.go:117`
- Modify: `internal/dialer/transport/http/session_handler.go` (resolver wiring if needed)
- Create or modify: `internal/dialer/api/<file>.go` (CallTenantResolver port — if not already existing)
- Modify: `internal/dialer/module.go` (resolve the new port via locator OR build inline)
- Test: `internal/dialer/transport/http/routes_test.go` (new test case: tenant-A admin posts hangup against tenant-B call → 404)

**Why third:** the Plan 13.2.5 cross-tenant audit explicitly flagged this endpoint and deferred the fix to v0.0.26. This plan IS v0.0.26 — close the loop. The TestSmoke_TenantIsolation scenario in Task 8 exercises this regression path end-to-end.

**Pattern reference:** Plan 13.2.5 introduced `pkg/middleware/tenant.RequireSameTenant(resolveFn)`. The resolver function signature is:
```go
type ResolveFn func(ctx context.Context, id uuid.UUID) (tenantID uuid.UUID, err error)
```
On mismatch with `auth.ClaimsFromContext(c).TenantID`, the middleware writes `404 Not Found` with no body (existence-probe defence).

For `:id/hangup`, the resolver is "look up `calls.tenant_id WHERE id = :id`". This requires either:
1. A new method on `dialer.Router` (or wherever the call's metadata is fetched) — e.g. `LookupCallTenant(ctx, callID) → (uuid.UUID, error)`.
2. A direct BypassRLS read against the `calls` table, packaged as `internal/dialer/api.CallTenantResolver` port.

**Check first:** does `internal/dialer/api/` already export a CallResolver port (Plan 11.4 wired `realtime.CallResolver` — the dialer side may share or mirror)? If yes, reuse. If no, add the smallest reasonable port. **DO NOT** invent a new shape without checking `internal/realtime/service/cached_call_resolver.go` first.

- [ ] **Step 1: Write the failing test**

In `internal/dialer/transport/http/routes_test.go` (extend the existing hangup table at line 574):
```go
func TestHangup_CrossTenant_Returns404(t *testing.T) {
    t.Parallel()
    f := newFixture(t)

    // Seed: a call belonging to tenant B exists; tenant A admin attempts hangup.
    tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
    tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
    callID := uuid.New()
    f.callTenantLookups[callID] = tenantB // fake resolver returns tenant B

    rec := f.doAuthAs(t, tenantA, "operator",
        stdhttp.MethodPost, "/api/calls/"+callID.String()+"/hangup", nil)

    assert.Equal(t, stdhttp.StatusNotFound, rec.Code,
        "cross-tenant hangup must 404 (no leak of existence)")
    assert.Empty(t, rec.Body.String(),
        "404 body must be empty per RequireSameTenant pattern")
    assert.Equal(t, 0, f.hangupCount, "no hangup should be dispatched")
}
```

The fixture `f.callTenantLookups` is a `map[uuid.UUID]uuid.UUID` that the resolver consults. Add to `newFixture` per the existing pattern. `doAuthAs(t, tenantID, role, ...)` is a helper that issues a JWT with the given tenant/role — extend the existing `doAuth` if needed.

- [ ] **Step 2: Run test to verify it fails**

```bash
go test -race -count=1 ./internal/dialer/transport/http/ -run TestHangup_CrossTenant_Returns404 -v
```
Expected: FAIL — no middleware, hangup dispatched regardless of tenant.

- [ ] **Step 3: Add the resolver port (if not existing) and middleware**

a) `internal/dialer/api/call_resolver.go` (new, if no existing port):
```go
package api

import (
    "context"

    "github.com/google/uuid"
)

// CallTenantResolver returns the tenant_id of a call by ID, using a
// BypassRLS scan because this is the input to a cross-tenant guard.
//
// Returns api.ErrCallNotFound if no row matches; the caller maps that
// to HTTP 404. Any other error is wrapped and surfaces as 500.
type CallTenantResolver interface {
    LookupCallTenant(ctx context.Context, callID uuid.UUID) (uuid.UUID, error)
}
```
Also add `ErrCallNotFound = errors.New("dialer: call not found")` to `internal/dialer/api/errors.go` if absent.

b) `internal/dialer/transport/http/routes.go:117` — wrap with the middleware:
```go
import (
    // ...
    tenantmw "github.com/sociopulse/platform/pkg/middleware/tenant"
)

calls.POST("/:id/hangup",
    tenantmw.RequireSameTenant(h.resolveCallTenant),
    h.hangup,
)
```

c) Add `h.resolveCallTenant(ctx, callID)` adapter in `session_handler.go`:
```go
func (h *handlers) resolveCallTenant(ctx context.Context, callID uuid.UUID) (uuid.UUID, error) {
    return h.callResolver.LookupCallTenant(ctx, callID)
}
```

d) Plumb `callResolver` into the handlers struct + constructor. The `dialer.Module` constructs it (PG-backed `internal/dialer/store/call_resolver.go` mirroring the Plan 11.4 realtime resolver pattern).

- [ ] **Step 4: Run test to verify it passes**

```bash
go test -race -count=1 ./internal/dialer/transport/http/ -run TestHangup -v
```
Expected: PASS for the new test AND for the existing hangup tests at line 574+.

- [ ] **Step 5: Confirm `make ci` is green**

```bash
make ci
go test -race -count=1 ./internal/dialer/...
gofmt -l internal/dialer/
make grep-time-after
```
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add internal/dialer/transport/http/routes.go internal/dialer/transport/http/session_handler.go internal/dialer/transport/http/routes_test.go internal/dialer/api/call_resolver.go internal/dialer/api/errors.go internal/dialer/store/call_resolver.go internal/dialer/module.go
git commit -m "$(cat <<'EOF'
fix(dialer/transport/http): guard POST /api/calls/:id/hangup against cross-tenant

Plan 21 Task 3 — closes the Plan 13.2.5 out-of-scope finding
("`POST /api/calls/:id/hangup` lacks tenant scope — tracked for Plan 14",
deferred to v0.0.26). Mirrors the Plan 13.2.5 pattern: middleware reads
:id from URL → BypassRLS resolve calls.tenant_id → compare with
auth.ClaimsFromContext.TenantID → 404 with no body on mismatch.

The CallTenantResolver port lives in internal/dialer/api/ to keep
the depguard module-boundary rules clean (transport reaches inward,
not across).

TestSmoke_TenantIsolation in Task 8 is the regression net.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `tests/smoke/` package — testcontainer stack + cmd/api boot harness

**Files:**
- Create: `tests/smoke/doc.go`
- Create: `tests/smoke/stack_test.go`
- Create: `tests/smoke/boot_test.go`
- Create: `tests/smoke/helpers_test.go`
- Create: `tests/smoke/README.md`
- Modify: `tests/README.md` (mention `smoke/`)

**Why fourth:** the harness is the prerequisite for every smoke scenario (Tasks 5–8). It boots Postgres + Redis + NATS testcontainers, applies migrations, provisions JetStream streams, writes a config pointing at the testcontainers, and runs `cmd/api/main.run(ctx, configDir)` as a goroutine. Each scenario test then talks to the API via real `http.Client`.

**Key design decisions** (from references file § 5):
- **Per-`TestMain` shared stack**: one Postgres + Redis + NATS container set, used by all smoke tests. Per-test isolation via DB-row cleanup helpers (`t.Cleanup(...) → TRUNCATE x, y, z CASCADE`).
- **cmd/api as goroutine, not as binary**: mirrors `cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly`. No `os/exec`.
- **Build tag `//go:build smoke`**: keeps the unit + integration suites unaffected.

- [ ] **Step 1: Write the failing test (smokeStack + bootAPI smoke + harness sanity)**

`tests/smoke/doc.go`:
```go
//go:build smoke

// Package smoke contains end-to-end "boot the whole cmd/api against a
// real Postgres+Redis+NATS testcontainer stack" tests.
//
// Run via:
//
//   go test -tags=smoke -race -count=1 -timeout=10m ./tests/smoke/...
//
// See README.md for the harness layout and CI integration notes.
package smoke
```

`tests/smoke/stack_test.go` — declares the `smokeStack` type and `newSmokeStack(t)` helper. It MUST:
- Start Postgres testcontainer (`postgres.Run(ctx, "postgres:16", ...)`)
- Start Redis testcontainer
- Start NATS testcontainer with JetStream enabled (`-js` flag in container args)
- Apply Postgres migrations via the `cmd/migrator` Go entry function (extract one if needed — see references § 2.5)
- Provision JetStream streams `tenant.>` and `trunks.>` (reuse the pattern from `cmd/api/main_test.go::ensureTestStream`)
- Expose DSNs / addresses via fields: `PostgresDSN string`, `RedisAddr string`, `NATSURL string`
- Provide a `Reset(t)` method that truncates per-tenant tables for per-test isolation

`tests/smoke/boot_test.go` — declares `bootAPI(t, stack) → (apiAddr string)`:
- Write a smoke config YAML pointing at `stack.PostgresDSN` / `stack.RedisAddr` / `stack.NATSURL`
- `pickFreeAddr(t)` for HTTP + metrics binds
- Run `cmd/api/main.run(ctx, configDir)` in a goroutine
- Wait for listener-ready via `listenerReadyChan`
- `t.Cleanup(cancel)` to drive graceful shutdown
- Return the API addr for tests to hit

`tests/smoke/helpers_test.go` — small HTTP helpers: `doRequest(t, method, url, body, headers) → *http.Response + body bytes`, `parseError(t, resp) → ErrorEnvelope`, `requireJSON(t, resp, &target)`.

`tests/smoke/health_test.go` — placeholder Task 5 sanity scenario added in this task as the harness shakedown:
```go
//go:build smoke

package smoke

import (
    "context"
    "net/http"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// TestSmoke_HarnessBootsAndHealthz exercises the harness end-to-end
// the minute it can. /healthz is wired by gateway middleware (Plan 02)
// and is on every cmd/api boot.
func TestSmoke_HarnessBootsAndHealthz(t *testing.T) {
    stack := getSharedStack(t) // initialised by TestMain
    addr := bootAPI(t, stack)

    req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/healthz", nil)
    require.NoError(t, err)
    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)
    t.Cleanup(func() { _ = resp.Body.Close() })

    assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestMain provisions the shared stack once for all smoke tests in
// this package. Per-test isolation is via stack.Reset(t) when needed.
func TestMain(m *testing.M) {
    code, err := runWithSharedStack(m)
    if err != nil {
        panic(err)
    }
    os.Exit(code)
}
```

`runWithSharedStack(m)` lives in `stack_test.go`. It builds the stack, runs `m.Run()`, terminates containers in `t.Cleanup` analogue (use `defer` since TestMain).

- [ ] **Step 2: Run test to verify it fails**

```bash
go test -tags=smoke -race -count=1 -timeout=10m ./tests/smoke/...
```
Expected: FAIL — the test file doesn't compile yet (or the harness pieces are stubs). The implementer fills in pieces until the test compiles and runs.

- [ ] **Step 3: Implement the harness pieces incrementally**

Build the harness in this order, running `go build -tags=smoke ./tests/smoke/...` after each:
1. `doc.go` + package skeleton — compiles
2. `helpers_test.go` HTTP helpers (no Postgres/Redis/NATS dep) — compiles
3. `stack_test.go` Postgres testcontainer — compiles + brings up a PG
4. Add Redis + NATS to stack — compiles + brings up the trio
5. Apply migrations via cmd/migrator Go entry — `stack.PostgresDSN` returns a migrated DB
6. JetStream stream provisioning — `stack.NATSURL` returns a NATS with `tenant.>` + `trunks.>` streams
7. `boot_test.go` writes config YAML + boots cmd/api as goroutine → returns addr
8. `health_test.go::TestSmoke_HarnessBootsAndHealthz` — runs end-to-end against the harness

If `cmd/migrator` doesn't expose a clean Go entry, refactor it as part of this task to expose `Run(ctx context.Context, cfg migrator.Config) error`. The refactor is small (the main() function already calls into a few clear steps).

- [ ] **Step 4: Run the harness test**

```bash
go test -tags=smoke -race -count=1 -timeout=10m ./tests/smoke/... -v -run TestSmoke_HarnessBootsAndHealthz
```
Expected: PASS. Container pulls add ~30–90 s the first time; cached subsequently. Total runtime <2 min.

If smoke flakes on port-bind races (see references § 4.2), document the workaround in `tests/smoke/README.md`. If testcontainers-go reaper hangs (§ 4.1), document `TESTCONTAINERS_RYUK_DISABLED=true` workaround.

- [ ] **Step 5: Confirm `make ci` is unaffected (smoke gated by build tag)**

```bash
make ci
go test -race -count=1 ./...   # no -tags=smoke
gofmt -l tests/smoke/
make grep-time-after
```
Expected: `make ci` green; no change in untagged test count; gofmt clean.

- [ ] **Step 6: Commit**

```bash
git add tests/smoke/ tests/README.md cmd/migrator/
git commit -m "$(cat <<'EOF'
test(smoke): add tests/smoke harness — testcontainer PG/Redis/NATS + cmd/api boot

Plan 21 Task 4 — opens the smoke layer described in
docs/architecture/10-end-to-end-testing-gaps.md § Phase 1.

The smoke package is gated by //go:build smoke so the default
make-ci unit + integration suites are unchanged. CI gets a dedicated
`smoke` job in Task 7.

Harness design (mirrors per-`TestMain` shared stack to keep the
per-test cost <250 ms):
- newSmokeStack(t) → Postgres+Redis+NATS testcontainers, migrations applied
- bootAPI(t, stack) → writes config, runs cmd/api/run as goroutine, returns addr
- helpers_test.go → http.Client helpers reused by every scenario

cmd/migrator gained a Run(ctx, cfg) Go entry point so the harness can
call it inline (avoids os/exec child-process bookkeeping).

TestSmoke_HarnessBootsAndHealthz is the harness shakedown — every
subsequent smoke scenario builds on this base.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: TestSmoke_HealthAndReadiness — sanity scenario

**Files:**
- Create: `tests/smoke/health_test.go` (replace the Task 4 placeholder with the full scenario)

**Why fifth:** asserts the harness sanity beyond `/healthz=200` — exercises `/readyz` (dependency liveness) and `/metrics` (Prometheus surface). This is the regression net for the gateway-middleware path the Plan 02 wiring established.

- [ ] **Step 1: Write the failing test**

`tests/smoke/health_test.go`:
```go
//go:build smoke

package smoke

import (
    "context"
    "encoding/json"
    "io"
    "net/http"
    "strings"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// TestSmoke_HealthAndReadiness asserts the gateway exposes /healthz,
// /readyz, /metrics on a full-stack boot. /readyz must report ALL
// expected dependencies healthy (Postgres/Redis/NATS), /metrics must
// emit at least one Prometheus-shaped metric line for the well-known
// counters wired in Plan 02.
func TestSmoke_HealthAndReadiness(t *testing.T) {
    stack := getSharedStack(t)
    addr := bootAPI(t, stack)

    ctx := t.Context()

    // /healthz — always OK on a booted gateway
    healthResp := mustGet(ctx, t, "http://"+addr+"/healthz")
    assert.Equal(t, http.StatusOK, healthResp.StatusCode)

    // /readyz — returns OK only when all registered checks pass.
    // With PG+Redis+NATS up and reachable, expect 200 + a body that
    // names each check by name with status="ok".
    readyResp := mustGet(ctx, t, "http://"+addr+"/readyz")
    require.Equal(t, http.StatusOK, readyResp.StatusCode,
        "readyz must succeed when Postgres, Redis, and NATS are reachable")
    body, _ := io.ReadAll(readyResp.Body)
    _ = readyResp.Body.Close()
    var ready struct {
        Status string `json:"status"`
        Checks []struct {
            Name   string `json:"name"`
            Status string `json:"status"`
        } `json:"checks"`
    }
    require.NoError(t, json.Unmarshal(body, &ready))
    assert.Equal(t, "ok", ready.Status)
    checkNames := map[string]string{}
    for _, c := range ready.Checks {
        checkNames[c.Name] = c.Status
    }
    assert.Equal(t, "ok", checkNames["postgres"], "postgres check missing or not ok: %v", checkNames)
    assert.Equal(t, "ok", checkNames["nats"], "nats check missing or not ok: %v", checkNames)
    // Redis check is module-owned (auth/dialer) — name may differ.
    // The presence assertion above on postgres+nats is sufficient.

    // /metrics — runs on the separate listener bound to metricsAddr.
    // bootAPI() returns the gateway addr; we need the metrics addr,
    // which the harness exposes via stack.MetricsAddr or similar
    // (Task 4 plumbs it). Verify a canonical metric is emitted.
    metricsResp := mustGet(ctx, t, "http://"+stack.MetricsAddrFor(addr)+"/metrics")
    require.Equal(t, http.StatusOK, metricsResp.StatusCode)
    metricsBody, _ := io.ReadAll(metricsResp.Body)
    _ = metricsResp.Body.Close()
    assert.True(t,
        strings.Contains(string(metricsBody), "go_goroutines") ||
            strings.Contains(string(metricsBody), "process_cpu_seconds_total"),
        "expected at least one well-known process metric — got body of len %d", len(metricsBody))
}

func mustGet(ctx context.Context, t *testing.T, url string) *http.Response {
    t.Helper()
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    require.NoError(t, err)
    cli := &http.Client{Timeout: 5 * time.Second}
    resp, err := cli.Do(req)
    require.NoError(t, err)
    return resp
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test -tags=smoke -race -count=1 ./tests/smoke/ -run TestSmoke_HealthAndReadiness -v
```
Expected: FAIL initially if /readyz or /metrics aren't exposed in the smoke config (rare — they're on by default) OR if `MetricsAddrFor` helper isn't implemented yet. If everything is in place, it MAY pass on the first run — this is acceptable; mark the test as a "characterization" if so, and document the rationale in code (`// characterization: pre-existing behaviour — covers gateway middleware regression`).

- [ ] **Step 3: Implement any harness gaps**

If `bootAPI` doesn't expose the metrics addr, plumb it through (`bootAPI` returns `(httpAddr, metricsAddr)`). Replace the `stack.MetricsAddrFor(addr)` placeholder in the test accordingly.

- [ ] **Step 4: Run test to verify it passes**

```bash
go test -tags=smoke -race -count=1 ./tests/smoke/ -run TestSmoke_HealthAndReadiness -v
```
Expected: PASS.

- [ ] **Step 5: `make ci` + race + gofmt**

```bash
make ci
gofmt -l tests/smoke/
```
Expected: green.

- [ ] **Step 6: Commit**

```bash
git add tests/smoke/health_test.go tests/smoke/boot_test.go
git commit -m "$(cat <<'EOF'
test(smoke): TestSmoke_HealthAndReadiness asserts gateway sanity

Plan 21 Task 5 — exercises /healthz, /readyz, /metrics against the
full-stack smoke boot. /readyz must report postgres + nats checks as
ok (Redis check is module-owned and may not show on this surface).
/metrics must emit at least one well-known process metric.

The regression net for the Plan 02 gateway middleware path; catches a
class of failure where a future refactor moves /readyz logic without
exercising the underlying checks.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: TestSmoke_AuthFullFlow — login → refresh → logout → expired-refresh-401

**Files:**
- Create: `tests/smoke/auth_test.go`
- Modify: `tests/smoke/seed_test.go` (new, or extend stack helper) — seeds a tenant + admin user via direct SQL
- Test: `tests/smoke/auth_test.go`

**Why sixth:** Auth is the entry point for every other smoke scenario. The full flow asserts:
1. `POST /api/auth/login` with valid creds returns 200 + access + refresh tokens.
2. `POST /api/auth/refresh` with the refresh token returns a fresh access token.
3. `POST /api/auth/logout` invalidates the refresh token.
4. `POST /api/auth/refresh` with the logged-out token returns 401.

**Seed data required:**
- One tenant row (`tenants` table) with a known `id`, `name`, `org_code` (e.g. `"SMOKE-TENANT-A"`).
- One admin user (`users` table) under that tenant: known login, argon2id-hashed password via `pkg/passwords.Default().Hash(plain)`, role=`admin`.
- KMS DEK / pepper rows as required by tenancy/auth — the local KMS provider materializes these from `cfg.KMS.LocalKeyHex` and `tenants.phone_hash_pepper`. Confirm migrations seed the pepper or insert it explicitly.

- [ ] **Step 1: Write the seed helpers**

`tests/smoke/seed_test.go`:
```go
//go:build smoke

package smoke

import (
    "context"
    "database/sql"
    "testing"

    "github.com/google/uuid"
    "github.com/stretchr/testify/require"

    "github.com/sociopulse/platform/pkg/passwords"
)

type SeedAccount struct {
    TenantID uuid.UUID
    UserID   uuid.UUID
    Login    string
    Password string
    Role     string
    OrgCode  string
}

func seedTenantAndAdmin(t *testing.T, stack *Stack, orgCode, login, plainPwd string) SeedAccount {
    t.Helper()
    ctx := t.Context()

    db, err := sql.Open("pgx", stack.PostgresDSN)
    require.NoError(t, err)
    t.Cleanup(func() { _ = db.Close() })

    tenantID := uuid.New()
    userID := uuid.New()
    hash, err := passwords.Default().Hash(plainPwd)
    require.NoError(t, err)

    _, err = db.ExecContext(ctx,
        `INSERT INTO tenants (id, name, org_code, phone_hash_pepper)
         VALUES ($1, $2, $3, $4)`,
        tenantID, "Smoke Tenant "+orgCode, orgCode, []byte("smoke-pepper-32bytes-do-not-use-prod"),
    )
    require.NoError(t, err)

    _, err = db.ExecContext(ctx,
        `INSERT INTO users (id, tenant_id, login, password_hash, roles, active)
         VALUES ($1, $2, $3, $4, ARRAY['admin']::text[], TRUE)`,
        userID, tenantID, login, hash,
    )
    require.NoError(t, err)

    t.Cleanup(func() {
        _, _ = db.ExecContext(context.Background(),
            `DELETE FROM users WHERE id = $1`, userID)
        _, _ = db.ExecContext(context.Background(),
            `DELETE FROM tenants WHERE id = $1`, tenantID)
    })

    return SeedAccount{
        TenantID: tenantID, UserID: userID, Login: login,
        Password: plainPwd, Role: "admin", OrgCode: orgCode,
    }
}
```

Note: column names + types MUST match the actual `tenants` and `users` schemas. Verify via `psql \d tenants` against a freshly migrated test DB before writing the SQL. The plan assumes `tenants(id, name, org_code, phone_hash_pepper bytea)` and `users(id, tenant_id, login, password_hash, roles text[], active boolean)`. Adjust if Plan 04 / 05 used different names.

- [ ] **Step 2: Write the failing auth test**

`tests/smoke/auth_test.go`:
```go
//go:build smoke

package smoke

import (
    "bytes"
    "encoding/json"
    "net/http"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

type loginReq struct {
    OrgCode  string `json:"org_code"`
    Login    string `json:"login"`
    Password string `json:"password"`
}
type loginResp struct {
    AccessToken  string `json:"access_token"`
    RefreshToken string `json:"refresh_token"`
    ExpiresIn    int    `json:"expires_in"`
}
type refreshReq struct {
    RefreshToken string `json:"refresh_token"`
}

func TestSmoke_AuthFullFlow(t *testing.T) {
    stack := getSharedStack(t)
    addr := bootAPI(t, stack)
    admin := seedTenantAndAdmin(t, stack, "SMOKE-A", "alice", "AlicePassword123!")

    ctx := t.Context()

    // 1. Login — happy path
    body, _ := json.Marshal(loginReq{
        OrgCode: admin.OrgCode, Login: admin.Login, Password: admin.Password,
    })
    req, err := http.NewRequestWithContext(ctx, http.MethodPost,
        "http://"+addr+"/api/auth/login", bytes.NewReader(body))
    require.NoError(t, err)
    req.Header.Set("Content-Type", "application/json")

    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)
    require.Equal(t, http.StatusOK, resp.StatusCode,
        "login must succeed for seeded admin")

    var lr loginResp
    require.NoError(t, json.NewDecoder(resp.Body).Decode(&lr))
    _ = resp.Body.Close()
    require.NotEmpty(t, lr.AccessToken, "access_token must be present")
    require.NotEmpty(t, lr.RefreshToken, "refresh_token must be present")

    // 2. Refresh — uses the refresh token from step 1
    rbody, _ := json.Marshal(refreshReq{RefreshToken: lr.RefreshToken})
    rreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
        "http://"+addr+"/api/auth/refresh", bytes.NewReader(rbody))
    require.NoError(t, err)
    rreq.Header.Set("Content-Type", "application/json")

    rresp, err := http.DefaultClient.Do(rreq)
    require.NoError(t, err)
    require.Equal(t, http.StatusOK, rresp.StatusCode,
        "refresh must succeed with a valid refresh_token")
    var lr2 loginResp
    require.NoError(t, json.NewDecoder(rresp.Body).Decode(&lr2))
    _ = rresp.Body.Close()
    assert.NotEmpty(t, lr2.AccessToken)
    assert.NotEqual(t, lr.AccessToken, lr2.AccessToken,
        "refresh must return a fresh access_token")

    // 3. Logout — invalidates the refresh token via Authorization header
    loreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
        "http://"+addr+"/api/auth/logout", nil)
    require.NoError(t, err)
    loreq.Header.Set("Authorization", "Bearer "+lr2.AccessToken)

    loresp, err := http.DefaultClient.Do(loreq)
    require.NoError(t, err)
    require.Equal(t, http.StatusNoContent, loresp.StatusCode,
        "logout must succeed (204 No Content)")
    _ = loresp.Body.Close()

    // 4. Refresh after logout — must 401
    rbody2, _ := json.Marshal(refreshReq{RefreshToken: lr.RefreshToken})
    rreq2, err := http.NewRequestWithContext(ctx, http.MethodPost,
        "http://"+addr+"/api/auth/refresh", bytes.NewReader(rbody2))
    require.NoError(t, err)
    rreq2.Header.Set("Content-Type", "application/json")

    rresp2, err := http.DefaultClient.Do(rreq2)
    require.NoError(t, err)
    assert.Equal(t, http.StatusUnauthorized, rresp2.StatusCode,
        "refresh after logout must 401 (session revoked)")
    _ = rresp2.Body.Close()
}
```

**Adjust to wire contract:** `/api/auth/login` request body shape is from `internal/auth/transport/http/login.go` — the field names may be `email` instead of `login`, or `org_id` instead of `org_code`. Check the actual handler before settling. Same for `/refresh` and `/logout`. The plan uses the `04-testing-strategy.md` example shape (`OrgID: "CC-MOSKVA-01"`, `Login: "alice"`); verify and adjust.

- [ ] **Step 3: Run test to verify it fails**

```bash
go test -tags=smoke -race -count=1 ./tests/smoke/ -run TestSmoke_AuthFullFlow -v
```
Expected: FAIL — exact failure depends on contract mismatch (typically `400 Bad Request` because the seed user's column shape differs from production migration).

- [ ] **Step 4: Iterate until passing**

Fix the contract mismatches discovered in Step 3. Common iterations:
- Field name (e.g. `org_id` vs `org_code`).
- Required columns missing from seed (e.g. `users.created_at` NOT NULL with no default).
- KMS DEK missing — for unencrypted seed columns this isn't needed; for TOTP it is. Phase 1 covers only password-login (no TOTP), so the DEK isn't on this path.

- [ ] **Step 5: `make ci` (untagged) + smoke tag**

```bash
make ci
go test -tags=smoke -race -count=1 ./tests/smoke/...
gofmt -l tests/smoke/
```
Expected: untagged green; smoke target green.

- [ ] **Step 6: Commit**

```bash
git add tests/smoke/auth_test.go tests/smoke/seed_test.go
git commit -m "$(cat <<'EOF'
test(smoke): TestSmoke_AuthFullFlow exercises login/refresh/logout

Plan 21 Task 6 — covers the auth happy path end-to-end against a
seeded tenant + admin user. Asserts:

  1. POST /api/auth/login returns 200 + access_token + refresh_token
  2. POST /api/auth/refresh returns a fresh access_token
  3. POST /api/auth/logout returns 204 (idempotent)
  4. POST /api/auth/refresh with the logged-out token returns 401

The seed helper writes one tenant + one admin user via direct SQL
(argon2id hash via pkg/passwords.Default().Hash). t.Cleanup deletes
the rows after the scenario, leaving the shared stack clean for the
next test.

Catches the JWT claims schema drift class (10-end-to-end-testing-gaps.md
failure scenario #3).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: TestSmoke_RbacEnforcement + TestSmoke_TenantIsolation

**Files:**
- Create: `tests/smoke/rbac_test.go`
- Create: `tests/smoke/isolation_test.go`
- Modify: `tests/smoke/seed_test.go` (add operator + tenantB seed helpers)

**Why seventh:** the two scenarios share seed code (multi-role users, two tenants) so they live in adjacent files and ship together. The smoke layer's value proposition is "catch wiring + middleware bugs"; these two scenarios are the canonical regressions.

**TestSmoke_RbacEnforcement:**
- Seed: admin user (role=admin) + operator user (role=operator) under SAME tenant.
- Operator logs in → POST `/api/projects` → expect 403 (RBAC matrix denies operator-on-projects-write).
- Admin logs in → POST `/api/projects` → expect 201.

**TestSmoke_TenantIsolation:**
- Seed: tenant A with admin A + a project A. Tenant B with admin B.
- Admin B logs in → GET `/api/projects/<projectA.ID>` → expect 404 (RLS swallows + RequireSameTenant middleware on the path).
- Admin B logs in → POST `/api/calls/<callA.ID>/hangup` → expect 404 (Task 3's regression net).

- [ ] **Step 1: Extend `seed_test.go` with multi-role + multi-tenant helpers**

Add functions: `seedOperator(t, stack, tenantID, login, password)`, `seedSecondaryTenant(t, stack, orgCode) → SeedAccount`, `seedProject(t, stack, tenantID, name) → uuid.UUID`, `seedCall(t, stack, tenantID, projectID) → uuid.UUID`.

The project + call helpers insert minimal-valid rows via direct SQL (matching the schemas from Plans 06 and 02). Confirm columns from `psql \d projects` and `psql \d calls` before writing the SQL.

- [ ] **Step 2: Write `rbac_test.go` failing test**

```go
//go:build smoke

package smoke

import (
    "bytes"
    "encoding/json"
    "net/http"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestSmoke_RbacEnforcement(t *testing.T) {
    stack := getSharedStack(t)
    addr := bootAPI(t, stack)
    admin := seedTenantAndAdmin(t, stack, "SMOKE-RBAC", "rbac-admin", "AdminPass123!")
    operator := seedOperator(t, stack, admin.TenantID, "rbac-operator", "OperatorPass123!")

    ctx := t.Context()
    adminJWT := loginAndGetAccessToken(ctx, t, addr, admin)
    operatorJWT := loginAndGetAccessToken(ctx, t, addr, operator)

    // operator → POST /api/projects → 403
    projBody, _ := json.Marshal(map[string]any{"name": "Smoke Project", "description": "smoke"})
    req, err := http.NewRequestWithContext(ctx, http.MethodPost,
        "http://"+addr+"/api/projects", bytes.NewReader(projBody))
    require.NoError(t, err)
    req.Header.Set("Authorization", "Bearer "+operatorJWT)
    req.Header.Set("Content-Type", "application/json")
    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)
    _ = resp.Body.Close()
    assert.Equal(t, http.StatusForbidden, resp.StatusCode,
        "operator must not be able to create projects (RBAC matrix)")

    // admin → POST /api/projects → 201
    req2, err := http.NewRequestWithContext(ctx, http.MethodPost,
        "http://"+addr+"/api/projects", bytes.NewReader(projBody))
    require.NoError(t, err)
    req2.Header.Set("Authorization", "Bearer "+adminJWT)
    req2.Header.Set("Content-Type", "application/json")
    resp2, err := http.DefaultClient.Do(req2)
    require.NoError(t, err)
    _ = resp2.Body.Close()
    assert.Equal(t, http.StatusCreated, resp2.StatusCode,
        "admin must be able to create projects")
}
```

`loginAndGetAccessToken` is a small helper extracted to `helpers_test.go` from Task 6's inline login flow.

- [ ] **Step 3: Write `isolation_test.go` failing test**

```go
//go:build smoke

package smoke

import (
    "net/http"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestSmoke_TenantIsolation(t *testing.T) {
    stack := getSharedStack(t)
    addr := bootAPI(t, stack)

    tenantA := seedTenantAndAdmin(t, stack, "SMOKE-ISO-A", "iso-admin-a", "PassA123!")
    tenantB := seedTenantAndAdmin(t, stack, "SMOKE-ISO-B", "iso-admin-b", "PassB123!")
    projectA := seedProject(t, stack, tenantA.TenantID, "Project A")
    callA := seedCall(t, stack, tenantA.TenantID, projectA)

    ctx := t.Context()
    jwtB := loginAndGetAccessToken(ctx, t, addr, tenantB)

    // Tenant B reads Tenant A's project → 404
    req, err := http.NewRequestWithContext(ctx, http.MethodGet,
        "http://"+addr+"/api/projects/"+projectA.String(), nil)
    require.NoError(t, err)
    req.Header.Set("Authorization", "Bearer "+jwtB)
    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)
    require.NoError(t, resp.Body.Close())
    assert.Equal(t, http.StatusNotFound, resp.StatusCode,
        "cross-tenant project read must 404 (RLS + RequireSameTenant)")

    // Tenant B attempts to hangup Tenant A's call → 404 (Task 3 regression net)
    hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
        "http://"+addr+"/api/calls/"+callA.String()+"/hangup", nil)
    require.NoError(t, err)
    hreq.Header.Set("Authorization", "Bearer "+jwtB)
    hresp, err := http.DefaultClient.Do(hreq)
    require.NoError(t, err)
    require.NoError(t, hresp.Body.Close())
    assert.Equal(t, http.StatusNotFound, hresp.StatusCode,
        "cross-tenant hangup must 404 (Plan 21 Task 3 regression net)")
}
```

- [ ] **Step 4: Run tests to verify they fail (or that the seed shape is wrong)**

```bash
go test -tags=smoke -race -count=1 ./tests/smoke/ -run 'TestSmoke_(Rbac|Tenant)' -v
```
Expected: at first, FAIL — typically because the seed function or the API endpoint shape is off. Iterate.

- [ ] **Step 5: Iterate seed helpers until both tests pass**

Pay particular attention to:
- `projects` table schema (the seed insert must satisfy all NOT NULL columns; check defaults).
- `calls` table schema — Plan 02 created it; many NOT NULL columns; you may need to seed via the dialer's `Router.Originate` flow OR via direct SQL with all required defaults filled.
- RLS on the seed inserts: direct SQL via the `app` user is subject to RLS. Either INSERT under `tenancy_admin` (BypassRLS) OR set `app.tenant_id` via `SET LOCAL`. Choose the latter for simpler test code.

If `seedCall` is too complex (requires too many minimal-valid columns), the alternative is to seed the call via the dialer's HTTP layer (post-login flow). But this couples the test to dialer's HTTP shape — direct SQL is preferred. Document the trade-off in the seed function's godoc.

- [ ] **Step 6: `make ci` + smoke tag**

```bash
make ci
go test -tags=smoke -race -count=1 ./tests/smoke/...
gofmt -l tests/smoke/
```
Expected: green.

- [ ] **Step 7: Commit**

```bash
git add tests/smoke/rbac_test.go tests/smoke/isolation_test.go tests/smoke/seed_test.go tests/smoke/helpers_test.go
git commit -m "$(cat <<'EOF'
test(smoke): TestSmoke_Rbac + TenantIsolation — cross-cutting regression nets

Plan 21 Task 7 — closes the auth-RBAC and cross-tenant scopes
end-to-end.

TestSmoke_RbacEnforcement asserts operator JWT → POST /api/projects
returns 403 (RBAC matrix) and admin JWT → 201. Catches a class of
failure where a future refactor breaks the RBAC fast-path or matrix
fallback.

TestSmoke_TenantIsolation seeds two tenants and asserts:
- Tenant B GET /api/projects/<A.id> → 404 (RLS swallows + RequireSameTenant)
- Tenant B POST /api/calls/<A.id>/hangup → 404 (Plan 21 Task 3 net)

This is the smoke-level proof that the Plan 13.2.5 cross-tenant audit
+ Plan 21 Task 3 fix are coherent. A future regression of the middleware
chain fails this test loudly.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: CI `smoke` job + close-out artifacts

**Files:**
- Modify: `.github/workflows/ci.yml` — add `smoke` job
- Modify: `tests/smoke/README.md` — CI matrix documentation
- Modify: `Makefile` — add `make test-smoke` convenience target

**Why eighth:** without CI integration, the smoke suite drifts. Per `09-agent-workflow-improvements.md` § Improvement #5: run on every push to `main` (parallel to deploy — channel-uniformly fast, not blocking the PR), plus the mandatory gate on `v*` tag push.

- [ ] **Step 1: Write the failing test (cron + workflow YAML scan)**

This task's "test" is the CI run itself — there's no Go test to write. The discipline is:
1. Add the workflow YAML.
2. Run it locally via `act` (if installed) OR push to a feature branch and watch.
3. Confirm green on PR-equivalent run.

If `act` isn't available, the implementer pushes to a feature branch named `plan-21-smoke-ci` and watches the run. This is the established pattern in the repo.

- [ ] **Step 2: Add the `smoke` job to `.github/workflows/ci.yml`**

Verify current `.github/workflows/ci.yml` structure (existing 6 jobs: lint / test / build / docker / vuln / secret-scan). Add a 7th job parallel to `test`:
```yaml
smoke:
  name: smoke (e2e)
  runs-on: ubuntu-latest
  needs: []        # parallel to test, not blocked by it
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: ${{ env.GO_VERSION }}
        cache: true
    - name: Pre-pull docker images
      run: |
        docker pull postgres:16
        docker pull redis:7
        docker pull nats:2.10
    - name: Run smoke suite
      env:
        TESTCONTAINERS_RYUK_DISABLED: "true"
      run: |
        go test -tags=smoke -race -count=1 -timeout=15m ./tests/smoke/...
    - name: Smoke trigger summary
      if: always()
      run: echo "smoke=${{ job.status }}"
```

Notes:
- `needs: []` makes it parallel to the other jobs.
- The pre-pull keeps the docker daemon cache warm — without this, the first test pulls and times out.
- `TESTCONTAINERS_RYUK_DISABLED=true` per `docs/references/plan-21-e2e-smoke-foundation.md` § 4.1.
- 15-min timeout absorbs cold-pull tail latency.
- On `v*` tag push the run is still gating because GitHub Actions evaluates `needs:` of the deploy/tag-publish job — confirm the deploy job in `ci.yml` has `needs: [..., smoke]` after this change, OR the deploy is in a separate workflow that should ALSO gate on `smoke` via `workflow_run` trigger.

- [ ] **Step 3: Verify `ci.yml` lints**

```bash
# If yamllint is installed:
yamllint .github/workflows/ci.yml
# Else, push to a branch and watch the parsed-YAML check from GitHub.
```
Expected: pass.

- [ ] **Step 4: Add `make test-smoke` convenience target**

In `Makefile`:
```make
.PHONY: test-smoke
test-smoke:
	go test -tags=smoke -race -count=1 -timeout=15m ./tests/smoke/...
```
And document in `tests/smoke/README.md`:
```markdown
## Running locally

    make test-smoke

Requires Docker. Cold runs take ~90 s; cached ~30 s.

## CI

The `smoke` job runs on every push to `main` and on every `v*` tag push.
See `.github/workflows/ci.yml`. Tag-push deploy gates on smoke green.
```

- [ ] **Step 5: Push to a feature branch and watch CI**

```bash
git push origin HEAD:plan-21-smoke-ci
gh run watch $(gh run list --branch plan-21-smoke-ci --limit 1 --json databaseId -q '.[0].databaseId')
```
Expected: smoke job runs green within ~5–10 min. If red, diagnose + fix; do not merge into main.

- [ ] **Step 6: Merge to main + tag v0.0.26**

After the smoke job lands green on `plan-21-smoke-ci`:
```bash
git checkout main
git merge --no-ff plan-21-smoke-ci
git push origin main
```
Watch CI on main; the smoke job runs again in parallel.

```bash
git tag -a v0.0.26-e2e-smoke-foundation -m "Plan 21: E2E Smoke Foundation — tests/smoke + module wiring"
git push origin v0.0.26-e2e-smoke-foundation
gh run watch $(gh run list --branch main --limit 1 --json databaseId -q '.[0].databaseId')
```
Expected: 7 CI jobs all green (lint / test / build / docker / vuln / secret-scan / smoke).

- [ ] **Step 7: Close-out documentation updates**

Per CLAUDE.md rules #7 + #8:
1. Update `PROJECT_STATUS.md`:
   - New milestone row: `v0.0.26-e2e-smoke-foundation | Plan 21 | YYYY-MM-DD | E2E smoke + module wiring`.
   - Move 🎯 NEXT pointer (Plan 01 stays where it was — still infra; mention in the row that the platform side is "smoke-foundation complete").
   - Add standing rules for: smoke harness pattern; tenancy/auth/crm/surveys wiring order in cmd/api; per-`TestMain` shared stack decision; testcontainers reaper-disabled flag.
2. Update `docs/references/plan-21-e2e-smoke-foundation.md` § 6 Production lessons:
   - Container startup timing on CI vs local
   - Any wiring gotchas surfaced in Tasks 1–3
   - Test flakes + fixes (port-bind races, JetStream stream lag, etc.)
   - testcontainers-go API quirks discovered via context7
   - Whether per-`TestMain` shared-stack decision held up
3. Update the `## Amendments` section in this plan file (replace `## Amendments: none` if reality diverged).

Commit the doc updates:
```bash
git add PROJECT_STATUS.md docs/references/plan-21-e2e-smoke-foundation.md docs/superpowers/plans/2026-05-15-21-e2e-smoke-foundation.md
git commit -m "$(cat <<'EOF'
docs: Plan 21 close-out (v0.0.26-e2e-smoke-foundation)

Plan 21 ships the Phase 1 of the E2E testing closure plan:
- tenancy/auth/crm/surveys wired into cmd/api (Plan 14 follow-up #1)
- POST /api/calls/:id/hangup cross-tenant guard (Plan 13.2.5 follow-up)
- tests/smoke/ package with 4 scenarios (Health/Auth/RBAC/TenantIsolation)
- smoke job in CI parallel to test

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
git push origin main
```

---

## Self-review (run after writing the plan above)

This section is the controller's pre-execute audit — a checklist run before dispatching the first implementer subagent.

**1. Spec coverage:** Each Phase 1 scenario in `10-end-to-end-testing-gaps.md` § Phase 1 is covered:
- Scenario "HealthAndReadiness-equivalent" — Task 5 (sanity).
- Scenario 1 (AuthFullFlow) — Task 6.
- Scenario 6 (RbacEnforcement) — Task 7.
- Scenario 7 (TenantIsolation) — Task 7 (also subsumes Task 3's regression net).
- Out-of-scope scenarios (2, 3, 4, 5, 8) are explicitly listed in the plan's "What's intentionally out of scope" section.
- CI integration — Task 8.
- Module wiring pre-step — Tasks 1–2.
- Plan 13.2.5 + Plan 14 carry-over fix — Task 3.

**2. Placeholder scan:** None remain. Each task has explicit file paths, complete code blocks, expected commands + outputs, and HEREDOC commit messages. No `TODO`, `TBD`, `similar to Task N`, or `appropriate error handling` phrases.

**3. Type / signature consistency:**
- `seedTenantAndAdmin(t, stack, orgCode, login, plainPwd) → SeedAccount` — same signature across Tasks 6 + 7. ✓
- `bootAPI(t, stack) → addr` — Task 4 defines; Tasks 5 + 6 + 7 consume. ✓
- `loginAndGetAccessToken(ctx, t, addr, admin)` — defined in Task 7 (extracted from Task 6 inline flow); the docstring above flags this. ✓
- `mustGet(ctx, t, url)` — Task 5 defines; future scenarios can reuse. ✓
- `CallTenantResolver.LookupCallTenant(ctx, callID) → (uuid.UUID, error)` — Task 3 defines; tests reference. ✓
- `RequireSameTenant(resolveFn)` — from `pkg/middleware/tenant` (Plan 13.2.5); Task 3 reuses without redefinition. ✓
- `pkg/passwords.Default().Hash(plain)` — referenced in Task 6 seed; verified path in `pkg/passwords/hasher.go`. ✓

**4. Vocabulary check:** Every term used (Tenant, Operator, Admin, RLS, FSM, RBAC, JWT, Refresh, Logout, Hangup, KEK, DEK, Outbox, JetStream, testcontainer) is from `CONTEXT.md` or is a project-conventional library/protocol name. No drift.

**5. Path adaptation:** Older plans say `internal/<X>` where `pkg/<X>` is correct in some cases. This plan checks the actual paths:
- `pkg/middleware/tenant` is a `pkg/` path (Plan 13.2.5 confirmed). ✓
- `internal/dialer/transport/http/routes.go` is correct (`internal/`). ✓
- `internal/auth/module.go` is correct. ✓
- `pkg/passwords/hasher.go` is correct (Plan 00a moved passwords to `pkg/`). ✓
- `cmd/api/main.go` + `cmd/api/main_test.go` correct. ✓

**6. ADR consistency:** No existing ADR is contradicted. The smoke harness reaffirms ADR-0015 (TDD mandatory) by extending the test pyramid; it does not introduce a new architectural choice that needs a new ADR.

**7. Pre-commit gate per task:** Each task ends with `make ci` + race-detector + gofmt + `make grep-time-after`. ✓

**8. References file exists:** [`docs/references/plan-21-e2e-smoke-foundation.md`](../../references/plan-21-e2e-smoke-foundation.md) — created with this plan, referenced in the plan header. ✓

**9. No cross-repo edits:** All edits live in `sociopulse-platform`. The smoke harness has no dependency on `sociopulse-web` or `sociopulse-infra`. ✓

If any issue surfaces during execution (e.g. an existing `internal/dialer/api/CallResolver` is named differently than this plan assumed), the implementer files a one-line amendment under the `## Amendments` section of this plan at close-out.

---

## Execution handoff

Plan complete. To execute:

**Recommended approach:** `superpowers:subagent-driven-development` (controller dispatches one implementer subagent per task, runs 2-stage review per task, tracks via TodoWrite).

Per task, the implementer prompt MUST contain:
1. Full task text from this file (NOT the path — the implementer should not re-read the plan file).
2. Path to `docs/references/plan-21-e2e-smoke-foundation.md` with "READ FIRST" instruction.
3. Explicit references to relevant skills: `golang-testing` (for testcontainer patterns), `golang-context` (for context propagation in tests), `golang-concurrency` (for goroutine-driven cmd/api boot — BP1-BP9), `golang-error-handling` (for error wrapping in helpers).
4. TDD requirement: `superpowers:test-driven-development`, Red → Green → Refactor.
5. `context7` MCP requirement for any library API check (testcontainers-go has API churn).
6. `WebSearch` for unfamiliar errors / Docker daemon quirks.
7. Quality bar: `make ci` + `go test -race -count=1` + `gofmt -l` + `make grep-time-after` ALL green before reporting DONE.
8. MUST commit at the end of the task (per repo convention).

Re-review proportionality (per `09-agent-workflow-improvements.md` § Improvement #7):
- ≤ 5 lines + tickbox match → controller fixes inline, no re-review.
- 6–30 lines → spec-only re-review.
- > 30 lines OR new public symbols OR new tests → full 2-stage re-review.

# Plan 21b — Phase-1b smoke scenarios (project import / operator WS / surveys / recording / 152-ФЗ purge)

> **Subagents must read this file BEFORE writing code.** Captures canonical
> specs, the verified state of the codebase, and the gotchas Phase-1b
> scaffolding will hit the first time. Plan file at
> `docs/superpowers/plans/2026-05-15-21b-phase-1b-smoke-scenarios.md`
> tells you WHAT to write; this file tells you WHERE the rakes are.

## 1. Canonical specs

- **Closure plan (master design):** [`docs/architecture/10-end-to-end-testing-gaps.md`](../architecture/10-end-to-end-testing-gaps.md)
  - § "Phase 1 — `tests/smoke/`" lists 8 scenarios. Plan 21 (`v0.0.26-e2e-smoke-foundation`) delivered scenarios 1, 6, 7 + a Health/Readiness warm-up. Plan 21b delivers the remaining FIVE: 2 (admin import), 3 (operator WS state broadcast), 4 (surveys CRUD), 5 (recording stream), 8 (152-ФЗ soft-delete + purge).
  - § "Why this matters — concrete failure scenarios" #4 (migration RLS/grant drift), #5 (HTTP middleware order), #6 (resolver-cache invalidation drift) are the gap classes Phase-1b's scenarios close.
- **Plan 21 references (foundation + gotchas):** [`docs/references/plan-21-e2e-smoke-foundation.md`](plan-21-e2e-smoke-foundation.md) — read sections 2.4–2.9 + 4.1–4.7 + the entire "Production lessons" block. Plan 21b BUILDS on Plan 21's harness; do not reinvent shared-stack, ryuk-disable, JetStream pre-provisioning, JWT-secret config, KMS=local config, etc.
- **Testing strategy:** [`docs/architecture/04-testing-strategy.md`](../architecture/04-testing-strategy.md)
  - § "Layer 2 — Integration" — testcontainer canon Phase-1b reuses (per-test isolation tightening: shared TestMain stack + per-test Reset/seed cleanup is the established pattern).
  - § "What this strategy does NOT yet cover" — names `tests/smoke/` as the remaining gap; Phase-1b finishes Phase 1 of the 5-phase closure.
- **TDD discipline:** [`docs/architecture/08-tdd-discipline.md`](../architecture/08-tdd-discipline.md) + ADR-0015. Smoke-test TDD: RED = scenario fails because the target endpoint / wiring isn't reachable; GREEN = wire/seed it; REFACTOR = extract harness helpers.
- **System-design spec:** `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`
  - §17 — testing pyramid (Phase-1b stays at the smoke layer; load/chaos is Phase 5).
  - §15 (recording) — ciphertext-sha256 chain-of-custody + Range-header rejection rationale.
  - §11 (crm) — respondent soft-delete + 30-day purge window (152-ФЗ).
- **ADRs:**
  - ADR-0005 (recording integrity 99.5%) — informs the scenario-5 sha256 assertion.
  - ADR-0010 (NATS JetStream) — durable subjects; smoke pre-provisions `tenant.>` + `trunks.>` BEFORE cmd/api boot (mirrors Plan 21 `EnsureSmokeStreams`).
  - ADR-0014 (gin), ADR-0015 (TDD mandatory).
- **Domain glossary:** [`CONTEXT.md`](../../CONTEXT.md) — vocabulary canon. Test names use glossary terms (`Operator`, `Respondent`, `FSM`, `Recording`, `RLS`, `152-ФЗ`).

## 2. Reality-checked codebase state (verified 2026-05-15)

### 2.1 Plan 21 harness — what to reuse vs extend

| Existing in `tests/smoke/` (Plan 21) | Reuse as-is | Extend in Plan 21b |
|---|---|---|
| `Stack{PostgresDSN, RedisAddr, NATSURL}` | yes | (no new container fields needed — see § 2.5) |
| `GetSharedStack(t)` per-`TestMain` singleton | yes | — |
| `NewSmokeStreams` / `EnsureSmokeStreams` (JetStream pre-provision) | yes | — |
| `WriteSmokeConfig(t, stack, httpAddr, metricsAddr)` | yes (need to add fields for cmd/worker config) | one new field: `Worker` block (asynq Redis is already in cmd/api config; cmd/worker reuses the same DSN/Addr) |
| `PickFreeAddr(t)` / `ListenerReadyChan(addr, timeout)` | yes | — |
| `SeedTenantAndAdmin(t, ..., orgCode, login, plainPwd)` | yes | — |
| `SeedOperator(t, ..., tenantID, login, plainPwd)` | yes | — |
| `SeedProject(t, ..., tenantID, code, name)` | yes | — |
| `SeedCall(t, ..., tenantID, projectID)` | yes | — |
| `Stack.Reset(t)` (no-op stub today) | wire it | implement TRUNCATE-style cleanup for new tables Phase-1b touches: `respondents`, `respondent_imports` (or wherever the import job state lives), `operator_state`/`operator_sessions`, `surveys` + `survey_versions`, `call_recordings` |
| `init()` setting `TESTCONTAINERS_RYUK_DISABLED=true` | yes | — |
| `cmd/api/smoke_test.go::bootAPI(t, stack) (httpAddr, metricsAddr string)` | yes | — |

**New helpers Plan 21b must add** (under `tests/smoke/`):
- `tests/smoke/wsclient.go` — `coder/websocket` client wrapper (`DialOperator(ctx, t, addr, jwt) (*WSConn, error)` returning a wrapper around `*websocket.Conn` with `ReadJSON(out)` / `Close()`).
- `tests/smoke/asynqboot.go` — boots cmd/worker as a goroutine in-process (mirrors `bootAPI`'s pattern; calls `cmd/worker/main.run(ctx, configDir)`). Returns `(healthzAddr string, cleanup func())`.
- `tests/smoke/recording_seed.go` — encrypts a fixture audio blob with the smoke `LocalKMSClient`, puts the ciphertext into a smoke-owned `LocalObjectStore`, and inserts the matching `call_recordings` row. **The same `LocalObjectStore` instance is injected into cmd/api via the smoke override seam (§ 2.6).**
- `tests/smoke/survey_seed.go` — minimal valid survey schema fixture (one question; passes the `internal/surveys/api/validator.Validator`). Inserts `surveys` row + initial `survey_versions` draft via direct SQL.
- `tests/smoke/respondent_helpers.go` — multipart CSV builder for the import scenario; polling helper `WaitForImportStatus(t, addr, jwt, jobID, target string)`.
- `tests/smoke/clock.go` — purge-scenario-only helper that returns `func() time.Time` returning `time.Now() + 31 days` (used to build a smoke-owned `crmservice.PurgeWorker` directly, NOT injected into cmd/worker — see § 2.4).

### 2.2 Verified routes (Phase-1b scenarios touch ALL of these)

| Scenario | HTTP method + path | Verified by |
|---|---|---|
| 2 (import) | `POST /api/projects/:id/respondents/import` (multipart `file=…`, `?format=csv&filename=…`) | `internal/crm/transport/http/routes.go:83` + `respondent_handler.go:189` |
| 2 (import status) | `GET /api/imports/:job_id` (admin) | `internal/crm/transport/http/routes.go:96` + `respondent_handler.go:264` |
| 3 (operator WS) | `GET /api/operator/ws?token=<jwt>` (NOT `/api/dialer/sessions/me/ready` as gap-doc claimed — that endpoint does not exist; the dialer module mounts on `/api`, not `/api/dialer`) | `internal/dialer/transport/http/routes.go:147` + `internal/dialer/module.go:534` (mount point) |
| 3 (operator goes ready) | `POST /api/sessions/start` (NOT `/api/dialer/sessions/me/ready`) | `internal/dialer/transport/http/routes.go:119` |
| 3 (operator pause) | `POST /api/sessions/pause` | `routes.go:121` |
| 4 (surveys create) | `POST /api/surveys` (admin) | `internal/surveys/transport/http/routes.go:57` |
| 4 (surveys preview) | `POST /api/surveys/:id/preview/run` (operator+) | `routes.go:52` |
| 4 (save version) | `POST /api/surveys/:id/versions` (admin) | `routes.go:60` |
| 4 (activate version) | `POST /api/surveys/:id/versions/:version_id/activate` (admin) | `routes.go:61` |
| 4 (validate) | `POST /api/surveys/:id/validate` (admin) | `routes.go:62` |
| 5 (recording stream) | `GET /api/calls/:id/recording` (admin/supervisor) — NOT `/api/recordings/:id/stream` as gap-doc claimed | `internal/recording/transport/http/routes.go:64` |
| 5 (recording search) | `GET /api/recordings/search` (admin/supervisor) | `routes.go:67` |
| 8 (respondent delete) | `DELETE /api/respondents/:id` (admin) | `internal/crm/transport/http/routes.go:90` |

**Path-correction lessons re-applied here:** the gap-doc paths were aspirational; what's actually mounted differs. Verify-before-assert on every new scenario.

### 2.3 cmd/worker boot seam — already exists and matches cmd/api pattern

`cmd/worker/main.go:137 — func run(ctx context.Context, configDir string) error`. Same shape as `cmd/api/main.go:105`. The smoke harness mirrors `bootAPI` for `bootWorker`:

```go
// tests/smoke/asynqboot.go (NEW in Plan 21b)
func BootWorker(t *testing.T, stack *Stack) (healthzAddr string) {
    t.Helper()
    healthzAddr = PickFreeAddr(t)
    configDir := WriteSmokeWorkerConfig(t, stack, healthzAddr) // healthz only — no HTTP/Metrics

    ctx, cancel := context.WithCancel(context.Background())
    errCh := make(chan error, 1)
    go func() { errCh <- workerRun(ctx, configDir) }() // workerRun is a thin re-export shim under cmd/worker/

    t.Cleanup(func() {
        cancel()
        select {
        case <-errCh:
        case <-time.After(10 * time.Second):
            t.Errorf("smoke: worker run() did not exit within 10s of cancel")
        }
    })

    select {
    case err := <-errCh:
        t.Fatalf("smoke: worker run() returned before healthz was ready: %v", err)
    case <-ListenerReadyChan(healthzAddr, 30*time.Second):
    }
    return healthzAddr
}
```

**The shim:** `cmd/worker/run_smoke.go` (build-tagged `//go:build smoke`) exports `func RunSmoke(ctx context.Context, configDir string) error { return run(ctx, configDir) }`. Importing it into `tests/smoke/asynqboot.go` keeps `cmd/worker/main.run` unexported in production builds. Mirrors how Plan 21 placed `cmd/api/smoke_test.go` INSIDE the cmd/api package to access `run()` — but tests/smoke can't import package `main`, so the shim is the seam.

Verified by: `cmd/worker/main.go:137` + the existing `cmd/worker/main_integration_test.go::TestRunHappyPathStartsAndStops` which already calls `run(ctx, configDir)` from inside the package.

### 2.4 Scenario 8 (152-ФЗ purge) — clock injection ALREADY exists; do NOT refactor production code

`internal/crm/service/purge.go:89` — `NewPurgeWorker(pool, store, audit, grace, batch, clock func() time.Time)`. The 6th param is the clock seam. `nil` falls back to `time.Now`. Plan 21b's scenario-8 path:

1. Smoke test seeds tenant+admin+respondent
2. HTTP `DELETE /api/respondents/:id` via cmd/api → row gets `deleted_at = NOW()`
3. Smoke test directly constructs `crmservice.NewPurgeWorker(pool, store, audit, 30*24*time.Hour, 1000, func() time.Time { return time.Now().Add(31*24*time.Hour) })`
4. Calls `worker.Run(ctx)` directly
5. Asserts the row is physically gone via `SELECT count(*) FROM respondents WHERE id = $1`

**No cmd/worker boot needed for scenario 8.** The PurgeWorker is a plain struct with `Run(ctx) error`; the in-process construction reuses the same Postgres pool the cmd/api side wrote through. This sidesteps the asynq cron entirely.

The `pool purgeBypassRunner` interface only needs `BypassRLS(ctx, fn) error` — the smoke harness already has a `*postgres.Pool` from cmd/api boot (or constructs one via `pkg/postgres.New(cfg)` from the smoke config). The `auditapi.Logger` can be a no-op fake (`audit.NoopLogger{}` if exists; else write a 3-line stub in `tests/smoke/audit_noop.go`).

Verified by: `internal/crm/service/purge.go:60-101` (constructor signature + nil-handling) + `internal/crm/service/purge_test.go` (canonical test pattern shows direct construction without asynq).

### 2.5 MinIO is NOT needed for Phase-1b — use the existing LocalObjectStore

The original gap-doc text said "pre-encrypted .opus.enc in MinIO" for scenario 5 + "MinIO testcontainer" for scenario 2. Re-evaluation:

- **Scenario 2** (admin import): the import request is multipart-CSV upload via HTTP; the result rows go into `respondents` table. **No S3 round-trip on the happy path.** MinIO would test a code path that doesn't exist in the import flow. NOT needed.
- **Scenario 5** (recording stream): cmd/api's recording module today uses `LocalObjectStore` (in-memory) — Yandex S3 adapter is Plan 01 territory. MinIO would test a non-existent code path. NOT needed for Phase-1b; graduates to MinIO when Plan 01 lands a real S3 adapter.

**Decision:** Phase-1b uses `internal/recording/storage.LocalObjectStore` for scenario 5 + the existing multipart-CSV path for scenario 2. **No MinIO testcontainer wiring.** Plan 01 will introduce the MinIO/Yandex-S3 path; the smoke harness's `Stack` grows an optional `S3Endpoint` field at THAT time.

This dramatically simplifies the harness scope. The Plan-21 reference file's § 2.7 anticipated this by recommending lazy/optional MinIO instantiation — Phase-1b stays in the "lazy / not instantiated" branch.

### 2.6 Scenario 5 — LocalObjectStore injection seam (smoke-only)

cmd/api builds `recordingPorts` at `cmd/api/main.go:312` via `recwire.LocalPorts(cfg.Recording, logger)`. To pre-seed the in-memory blob from the smoke test, we need a smoke-tagged override.

**The seam (NEW file, build-tagged):** `cmd/api/smoke_overrides.go` (`//go:build smoke`):
```go
//go:build smoke

package main

import recwire "github.com/sociopulse/platform/internal/recording/wire"

// smokeOverrideRecordingPorts is consulted by run() before calling
// recwire.LocalPorts when the smoke build tag is active. Smoke tests
// populate it via SetSmokeRecordingPorts BEFORE invoking bootAPI so the
// cmd/api process and the test share one *LocalObjectStore instance —
// the test pre-encrypts a fixture, Puts it under (bucket,key), and the
// HTTP recording-stream handler reads the same bytes back.
//
// The variable is declared in a build-tagged file so production builds
// (no smoke tag) do not carry the test-only field.
var smokeOverrideRecordingPorts *recwire.Ports

// SetSmokeRecordingPorts is the test-side setter. Build-tagged so
// production code cannot accidentally call it.
func SetSmokeRecordingPorts(p *recwire.Ports) {
    smokeOverrideRecordingPorts = p
}
```

**The use-site (production code, no tag change):** `cmd/api/main.go:312` is wrapped with a tiny indirection function `buildRecordingPorts(cfg, logger)` that returns `smokeOverrideRecordingPorts` (when not nil) or falls through to `recwire.LocalPorts(...)`. The `smokeOverrideRecordingPorts` symbol is declared in a build-tagged file → production builds get the always-nil variant via a parallel `cmd/api/smoke_overrides_prod.go` (`//go:build !smoke`):
```go
//go:build !smoke

package main

import recwire "github.com/sociopulse/platform/internal/recording/wire"

var smokeOverrideRecordingPorts *recwire.Ports = nil
```

Mirrors the canonical Go build-tag stub-vs-real adapter pattern (PROJECT_STATUS.md "Stub-vs-real adapter pattern"). The `_ = smokeOverrideRecordingPorts` reference in production keeps the symbol live without a runtime cost.

### 2.7 Scenario 5 — sha256 chain-of-custody contract

`call_recordings.sha256` is the **sha256 of the CIPHERTEXT** (per Plan 12.1 commit-audit + Plan 12.4 integrity worker). The HTTP search response surfaces it; the stream response decrypts back to plaintext. The scenario's assertion:

```go
// 1. Compute sha256 of the ciphertext we put in LocalObjectStore (NOT the plaintext)
ciphertextSHA := sha256.Sum256(encryptedAudioBlob)
hexSHA := hex.EncodeToString(ciphertextSHA[:])

// 2. Insert call_recordings with sha256 = hexSHA
// 3. GET /api/recordings/search → response includes sha256 field; assert equality
// 4. GET /api/calls/:id/recording → 200 + plaintext audio bytes (response body length matches the plaintext fixture; sha256 of body matches the plaintext fixture sha256)
// 5. GET /api/calls/:id/recording with `Range: bytes=0-1023` → 416 Range Not Satisfiable (per ADR-0005 §15.4 — Range deliberately rejected to keep DEK lifetime bounded to one full read)
```

Verified by: `migrations/000001_init.up.sql:create table call_recordings` (`sha256 text not null`) + `internal/recording/transport/http/*.go` (search response shape, stream handler behaviour).

### 2.8 Scenario 4 — surveys schema + `survey_versions_active_one` partial-unique constraint

The activation flow asserts ONE active version per survey. The constraint enforces it at the DB level. The scenario:

1. POST `/api/surveys` → 201 `{id}` (admin)
2. POST `/api/surveys/:id/versions` with a minimal valid schema → 201 `{version_id, semver: "1.0"}` (admin)
3. POST `/api/surveys/:id/versions/:version_id/activate` → 200 (admin)
4. (Regression) POST `/api/surveys/:id/versions` with another schema → save another version → activate it → 200; query `survey_versions WHERE survey_id=:id AND is_active=true` returns exactly ONE row (the newer one).

The minimal valid schema body shape: needs to pass `internal/surveys/service/validator.Validator.Validate(schema)`. **Verify the exact JSON shape by reading `internal/surveys/api/dto.go::SaveVersionRequest` + `internal/surveys/service/validator.go` BEFORE writing the test.** The fixture lives in `tests/smoke/survey_seed.go`.

### 2.9 Scenario 3 — operator WS auth via `?token=` (NOT Authorization header)

Per `internal/dialer/transport/http/routes.go:141-147` the comment says:
> "Operator real-time channel — mounted OUTSIDE the JWTMiddleware chain because browsers cannot easily set Authorization on a WebSocket handshake. The WS handler self-authenticates against Deps.Validator using the ?token= query parameter (with an Authorization-header fallback for non-browser clients)…"

So smoke `DialOperator` **must** put the JWT in the query string:
```go
url := fmt.Sprintf("ws://%s/api/operator/ws?token=%s", addr, jwt)
c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
    Subprotocols: []string{...}, // verify subprotocol from ws_adapter.go (sociopulse-v1?)
})
```

Verify subprotocol from `internal/dialer/transport/http/ws_adapter.go` or `internal/realtime/transport/http/ws_adapter.go` BEFORE writing the test.

### 2.10 Asynq subscriber wiring is already in place; smoke needs to boot the worker

`internal/crm/module.go` registers two asynq surfaces in cmd/api: import-job consumer + purge cron. Plan 21 fixed the asynq shared-Redis bug (`*FromRedisClient`). For scenario 2 (admin import), **cmd/api alone is enough** — the import job is processed by the asynq Server registered inside crm.Module. For scenario 8 (purge), use direct PurgeWorker.Run (§ 2.4).

**There is NO need to boot cmd/worker for scenarios 2 or 8.** cmd/worker's runtime is for the analytics/billing/decryptor/dialer-events/recording-retention paths — none of which Phase-1b's five scenarios exercise.

**Reconsider.** This means Phase-1b does NOT need `BootWorker`. § 2.3's `BootWorker` helper is **deferred** to a hypothetical Phase-1c (recording retention worker, decryptor, etc.). Removing it from Plan 21b's scope shrinks the plan further.

**Final harness extension list for Plan 21b:**
- `wsclient.go` (scenario 3)
- `recording_seed.go` (scenario 5)
- `survey_seed.go` (scenario 4)
- `respondent_helpers.go` (scenario 2)
- `clock.go` + `audit_noop.go` (scenario 8)
- `Stack.Reset(t)` implementation (all scenarios)
- cmd/api smoke override pair: `smoke_overrides.go` + `smoke_overrides_prod.go` (scenario 5)
- `Stack` does NOT grow new container fields.

## 3. Library / SDK references (use `context7` to verify current APIs)

| Library | Used for | context7 ID |
|---|---|---|
| `github.com/coder/websocket` + `wsjson` | Scenario 3 — WS dial + JSON read/write | `/coder/websocket` (verified — see § 5 example) |
| `github.com/hibiken/asynq` | Scenario 2 — already in cmd/api boot via crm.Module; tests inspect job state via `GET /api/imports/:job_id` (no asynq client needed in tests) | already in `go.mod` |
| `github.com/jackc/pgx/v5` | Direct seed + assertion SQL | already in `go.mod` |
| `github.com/google/uuid` | UUID generation in seeds | already in `go.mod` |
| `github.com/stretchr/testify` | `require`/`assert` | already in `go.mod` |
| `golang-migrate/migrate/v4` | Already wired by Plan 21 — no change | already imported in `tests/smoke/stack.go` |

**Rule (re-emphasis):** before writing code that calls a method on any library, run `mcp__plugin_context7_context7__resolve-library-id` then `query-docs` to confirm the current signature. Do NOT guess.

Verified `coder/websocket` API (from `/coder/websocket` context7 query):
```go
import (
    "github.com/coder/websocket"
    "github.com/coder/websocket/wsjson"
)
c, resp, err := websocket.Dial(ctx, "ws://addr/api/operator/ws?token=...", &websocket.DialOptions{
    Subprotocols: []string{"sociopulse-v1"}, // verify from ws_adapter
})
defer c.CloseNow()
var msg map[string]any
err = wsjson.Read(ctx, c, &msg)
c.Close(websocket.StatusNormalClosure, "")
```

## 4. Gotchas (known traps — read these BEFORE writing code)

### 4.1 The `tests/smoke/` package is read-only at the seed boundary

Plan 21's `SeedTenantAndAdmin` returns a `SeededAccount{TenantID, UserID, OrgCode, Login, Password, Role}`. **Plaintext password is intentional** — the test drives the login flow with it. Do NOT try to "improve" the helper by hashing earlier or moving the plaintext somewhere else; the harness is build-tagged smoke-only and the plaintext lives in the test process.

### 4.2 `respondents.deleted_at` semantics

Soft-delete sets `deleted_at = now()`. The PurgeWorker's `PurgeOlderThan(cutoff, batch)` deletes rows where `deleted_at < cutoff`. Scenario 8's clock pretends 31 days have passed → `cutoff = clock() - 30d` → rows with `deleted_at <= clock() - 30d` are purged. Verify the exact predicate by reading `internal/crm/store/respondent_store.go::PurgeOlderThan` BEFORE writing the test (the predicate is the one fact this scenario hangs on).

### 4.3 `respondent_imports` job-state surface

`GET /api/imports/:job_id` returns a status document. The exact fields differ from asynq's internal state. **Read `internal/crm/transport/http/dto.go::ImportStatusDTO` (line 172) BEFORE writing the polling helper.** The helper polls until `status` reaches the documented terminal state (likely "completed" or "succeeded" or similar — verify the literal value).

### 4.4 The CSV import format expects specific columns

The `?format=csv&filename=phones.csv` query carries the format hint. The CSV body shape is **defined by `internal/crm/service/import.go::parseCSV`** (or similar). Required columns + delimiter must match. Verify the exact format BEFORE writing the test (`grep -rn "csv.NewReader\|encoding/csv" /Users/user/call-center/sociopulse-platform/internal/crm/`).

### 4.5 KMS-encrypted phone numbers — fixture must use a tenant with a real KEK

Plan 21's `SeedTenantAndAdmin` uses an arbitrary `kms_kek_id` string ("smoke-kek-<orgCode>"). **For scenario 2** (which encrypts phone numbers via KMS during import), the smoke harness's `LocalKMSClient` MUST recognise this id. Either:
- (a) Pre-register the KEK in `WriteSmokeConfig`'s `LocalKMSConfig` map under the deterministic id, OR
- (b) Extend `SeedTenantAndAdmin` to mint a real KEK via the LocalKMSClient + persist its returned id

Plan-21 reference file § 2.6 flagged this as future work. **Plan 21b Task 3 (admin import scenario) is where (a) lands.** The KEK bytes can be 32-byte deterministic (`bytes.Repeat([]byte("X"), 32)`); the id mapping comes from a smoke-config field that mirrors production's `LocalKEKs map[string]string`.

### 4.6 The operator WS subprotocol + initial-frame contract

The dialer's `/api/operator/ws` handler sends a snapshot frame on first connect (per Plan 11 spec). Scenario 3 must:
1. Dial WS with the operator's JWT
2. Read the initial snapshot frame (`{"type":"snapshot",...}`) — this is the proof the WS pipeline is alive
3. Issue HTTP `POST /api/sessions/start` (operator becomes ready)
4. Read the next frame on the same WS — assert it's the state-change event with `state="ready"` (or whatever the canonical post-start state is)

**Verify the frame shape from `internal/realtime/api/events.go` or `internal/realtime/service/dispatcher.go` BEFORE writing the test.** The "state-change event" name / shape is the subscription contract; it can drift between plans.

### 4.7 cmd/api shutdown can take 5–10 s under smoke load

Plan 21's `bootAPI` cleanup uses a 10 s shutdown timeout. With Phase-1b scenarios doing more setup (WS dial + multipart upload + DB seed), the cleanup may need a longer fence. Default to 15 s; tune up if CI flakes.

### 4.8 Per-test isolation via `Stack.Reset(t)` is the v1 pattern

Phase-1b scenarios mutate more tables than Plan 21's. The `Stack.Reset(t)` stub (declared in Plan 21 `stack.go:91`) is implemented in this plan. The TRUNCATE list:

```sql
TRUNCATE
    respondents, respondent_imports,
    operator_sessions, operator_state_log,
    call_recordings, call_answers, calls,
    survey_versions, surveys
RESTART IDENTITY CASCADE;
```

Order is irrelevant under CASCADE but listing the leaves first reduces FK-cascade noise in pg_stat. The `tenants` + `users` rows survive Reset — they're owned by `t.Cleanup` from `SeedTenantAndAdmin`. Reset runs as `tenancy_admin` (BypassRLS) via the testcontainer superuser connection.

### 4.9 `make test-smoke` Makefile target

Plan 21 added a `test-smoke` target in `Makefile`. Plan 21b extends it ONLY if a new package needs to be discovered (it doesn't — every Phase-1b scenario lives under `cmd/api/smoke_test.go` or new `cmd/api/smoke_*_test.go` files). **Do NOT introduce a new top-level test package.** Reuse the package-`main` pattern from Plan 21.

### 4.10 testifylint discipline (re-emphasis)

`require.Positive(t, n)` over `require.Greater(t, n, int64(0))`. `require.ErrorIs(t, err, sentinel)` over `require.True(t, errors.Is(...))`. Plan 21 retro caught these inline; Plan 21b avoids them in the first commit.

## 5. Open questions (resolve before merging the plan)

1. **Are scenarios 2–8 each one task, or grouped?** Recommendation: ONE TASK PER SCENARIO. Each scenario has its own seed shape, assertion shape, and review surface. Five scenarios → five execution tasks, plus one upfront harness-extension task → six tasks total. Granularity matches Plan 21.
2. **Should scenario 5 (recording stream) include the 0-byte / 416 Range edge case?** Yes — ADR-0005 explicitly forbids Range requests, and the unit handler test already asserts the rejection. Smoke regression-net surfaces a future refactor that loosens the gate. Cost: +5 lines.
3. **Should scenario 2 (admin import) verify the encrypted phone bytes vs the plaintext that went in?** No — that's a unit/integration concern (`internal/crm/service/import_test.go` has it). Smoke asserts `respondents.count = N` after import completes. Anything more duplicates per-module coverage and adds flakes.
4. **Should scenario 8 (purge) test the asynq cron path too?** No — direct `PurgeWorker.Run` is the canonical exposed surface (`internal/crm/service/purge.go:127` doc comment); the asynq adapter is a thin shell `HandlePurgeTask → Run`. Testing `Run` directly is the contract; cron scheduling is asynq territory and out of scope for SoP-level smoke.
5. **Should `cmd/worker` boot land in Plan 21b for future-proofing?** No — Plan 21b's five scenarios don't need it. Add it when a recording-retention or decryptor scenario actually requires it. Doing it now creates dead helper code.

## 6. Production lessons (post-execution YYYY-MM-DD)

> Filled in at close-out per CLAUDE.md workflow rule #8. Until then, this section is empty by design — the plan-NN-*.md is read by future agents AS the curated reading list, not as a retrospective.

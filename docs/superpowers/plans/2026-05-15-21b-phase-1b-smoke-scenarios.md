# Plan 21b — Phase-1b smoke scenarios (project import / operator WS / surveys / recording / 152-ФЗ purge)

> Plan ID: 21b
> References: [`docs/references/plan-21b-phase-1b-smoke-scenarios.md`](../../references/plan-21b-phase-1b-smoke-scenarios.md) (**MUST be read by every implementer subagent BEFORE writing code**)
> Predecessor: [Plan 21](2026-05-15-21-e2e-smoke-foundation.md) (`v0.0.26-e2e-smoke-foundation`)
> Closure plan: [`docs/architecture/10-end-to-end-testing-gaps.md`](../../architecture/10-end-to-end-testing-gaps.md) — Plan 21 delivered Health/Auth/RBAC/TenantIsolation; Plan 21b delivers the remaining FIVE scenarios from the Phase-1 list (2, 3, 4, 5, 8).
> Testing strategy: [`docs/architecture/04-testing-strategy.md`](../../architecture/04-testing-strategy.md) § "What this strategy does NOT yet cover" — Phase-1b fully closes Phase 1 of the E2E gap.
> Affected ADRs (no contradictions): ADR-0005 (recording integrity 99.5%), ADR-0010 (NATS JetStream), ADR-0014 (gin), ADR-0015 (TDD mandatory). Scenario 5's Range-header assertion is the canonical ADR-0005 regression net.

## Amendments: none

## Goal

Close Phase 1 of the E2E smoke gap by adding the remaining FIVE scenarios that exercise the public HTTP/WS surface of cmd/api against a real testcontainer stack. The harness from Plan 21 (`tests/smoke/` + `cmd/api/smoke_test.go`) is the foundation; this plan extends it minimally — one WS client wrapper, one survey schema fixture, one recording-seed helper, one CSV-import helper, one clock helper + Reset implementation. **No new infrastructure dependencies** (no MinIO, no cmd/worker boot — see references file § 2.5–2.7 for the rationale).

**Success criteria** (mirrors `10-end-to-end-testing-gaps.md` § Success criteria):
- Five new `TestSmoke_*` scenarios live under `cmd/api/` and run green via `make test-smoke`.
- Every scenario asserts at least one cross-module contract (HTTP+DB, HTTP+NATS, HTTP+WS, HTTP+Crypto, HTTP+Worker-rerun).
- Existing four Plan-21 scenarios stay green (regression net).
- CI `smoke` job runtime stays ≤ 6 min cold, ≤ 90 s warm.
- `Stack.Reset(t)` is implemented (Plan 21 left it as a stub).

**Non-goals:**
- MinIO testcontainer (deferred to Plan 01 when Yandex S3 adapter lands).
- cmd/worker in-process boot (no Phase-1b scenario needs it; defer to Phase-1c if recording-retention / decryptor scenarios ever land in smoke).
- Real FreeSWITCH coverage (Plan 08 territory).
- Frontend Playwright (Plans 15-19 in `sociopulse-web`).

## Context (reality-checked 2026-05-15)

### Existing state (verified, with citation)

- **Plan 21 harness** is shipped at tag `v0.0.26-e2e-smoke-foundation`. `tests/smoke/{stack.go, config.go, helpers.go, jetstream.go, ports.go, seed.go, doc.go}` + `cmd/api/smoke_test.go` (4 scenarios green: Health/Auth/RBAC/TenantIsolation).
  Verified by: `tests/smoke/` ls + `git log --oneline -5`.

- **Module providers walk in cmd/api**: `tenancy → telephony → dialer → recording → analytics → reports → billing → auth → crm → surveys`. All 14 backend modules registered.
  Verified by: `cmd/api/main.go:339-346` (Plan 21 references § 2.1).

- **PurgeWorker has a clock seam already** (`internal/crm/service/purge.go:66-101`). Constructor takes `clock func() time.Time`; nil falls back to `time.Now`. **No production-code refactor needed for scenario 8.**
  Verified by: `internal/crm/service/purge.go:66-101` + `purge_test.go` canonical test pattern.

- **cmd/api recording ports flow**: `recwire.LocalPorts(cfg.Recording, logger)` at `cmd/api/main.go:312` builds `Ports{DEK, Objects}` once. Same instance is registered in the locator + passed to recording.Module's HTTP transport.
  Verified by: `cmd/api/main.go:304-328` + `internal/recording/wire/*.go`.

- **Routes verified** for every scenario (see references file § 2.2 table). Path-corrections from gap-doc:
  - Operator WS: `/api/operator/ws?token=...` (NOT `/api/dialer/sessions/me/ready`). Verified: `internal/dialer/transport/http/routes.go:147` + `internal/dialer/module.go:534`.
  - Operator goes ready: `POST /api/sessions/start`. Verified: `routes.go:119`.
  - Recording stream: `/api/calls/:id/recording` (NOT `/api/recordings/:id/stream`). Verified: `internal/recording/transport/http/routes.go:64`.
  - Surveys CRUD: `POST /api/surveys`, `POST /api/surveys/:id/versions`, `POST /api/surveys/:id/versions/:version_id/activate`. Verified: `internal/surveys/transport/http/routes.go:57-61`.
  - Respondents import: `POST /api/projects/:id/respondents/import` + `GET /api/imports/:job_id`. Verified: `internal/crm/transport/http/routes.go:83,96`.
  - Respondent soft-delete: `DELETE /api/respondents/:id`. Verified: `routes.go:90`.

- **call_recordings schema** (scenario 5): PK `call_id`, columns `tenant_id, s3_bucket, s3_key, duration_sec, sha256, codec, encrypted_dek, kms_key_id, retention_until, delete_at, created_at, committed_at, status, cold_at`.
  Verified by: `migrations/000001_init.up.sql` + `migrations/000010_recording_evolve.up.sql`.

- **`coder/websocket` API** (scenario 3): `websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols, HTTPHeader})` → `(*Conn, *http.Response, error)`. `wsjson.Read(ctx, c, &v)` / `wsjson.Write(ctx, c, v)`. `c.CloseNow()` / `c.Close(websocket.StatusNormalClosure, "")`.
  Verified by: `mcp__plugin_context7_context7__query-docs` against `/coder/websocket`.

### File structure (created / modified)

**New files** (8):
- `tests/smoke/wsclient.go` — `DialOperator(ctx, t, addr, jwt) (*OperatorWS, error)` + `(*OperatorWS).ReadEvent(ctx, timeout) (map[string]any, error)` + `Close()`. **`//go:build smoke`**.
- `tests/smoke/survey_seed.go` — `SeedSurvey(t, stack, tenantID, code, name) uuid.UUID` (inserts `surveys` row only — version flow happens via HTTP in the scenario). `MinimalValidSurveySchema() []byte` — fixture JSON passing the validator. **`//go:build smoke`**.
- `tests/smoke/recording_seed.go` — `BuildRecordingFixture(t, stack, tenantID, callID) RecordingFixture` (encrypts a deterministic Opus blob via the smoke KMS, returns `{Ciphertext, SHA256Hex, WrappedDEKHex, KMSKeyID, Plaintext, Bucket, Key}`). `SeedRecording(t, stack, tenantID, callID, fixture)` (inserts the `call_recordings` row + Puts ciphertext into the cmd/api–shared `LocalObjectStore` from `cmd/api.GetSmokeRecordingPorts()`). **`//go:build smoke`**.
- `tests/smoke/respondent_helpers.go` — `BuildCSVImport(rows [][]string) []byte` (writes CSV bytes matching the production import format). `WaitForImportStatus(t, addr, jwt, jobID, target string)` (poll-with-context until terminal status). **`//go:build smoke`**.
- `tests/smoke/clock.go` — `FutureClock(d time.Duration) func() time.Time` (returns a closure that returns `time.Now().Add(d)` deterministically). **`//go:build smoke`**.
- `tests/smoke/audit_noop.go` — `NoopAuditLogger struct{}` implements `auditapi.Logger` with empty no-op methods (used by the smoke-direct PurgeWorker construction in scenario 8). **`//go:build smoke`**.
- `cmd/api/smoke_overrides.go` — `//go:build smoke` — declares `var smokeOverrideRecordingPorts *recwire.Ports` + `SetSmokeRecordingPorts(*recwire.Ports)` + `GetSmokeRecordingPorts() *recwire.Ports`.
- `cmd/api/smoke_overrides_prod.go` — `//go:build !smoke` — declares `var smokeOverrideRecordingPorts *recwire.Ports = nil`. Mirrors stub-vs-real pattern.

**New test files** (5, one per scenario, under `cmd/api/`):
- `cmd/api/smoke_admin_import_test.go` — `TestSmoke_AdminCreatesProjectAndImportsRespondents`
- `cmd/api/smoke_operator_ws_test.go` — `TestSmoke_OperatorReadyAndStateBroadcast`
- `cmd/api/smoke_surveys_test.go` — `TestSmoke_SurveyCreatePreviewActivate`
- `cmd/api/smoke_recording_test.go` — `TestSmoke_RecordingSearchAndStream`
- `cmd/api/smoke_purge_test.go` — `TestSmoke_RespondentSoftDelete152FZ`

**Modified files** (4):
- `tests/smoke/stack.go` — `(s *Stack) Reset(t *testing.T)` body filled in (TRUNCATE list per references § 4.8). Also adds `Stack.smokeKEKID` (the deterministic KEK id baked into seeds for KMS-touching scenarios) and `Stack.PgPool() *postgres.Pool` accessor (scenario 8 needs a direct pool for the in-test PurgeWorker construction).
- `tests/smoke/config.go` — `WriteSmokeConfig` adds `recording.local_keks: { "smoke-kek-default": "<64-hex>" }` (deterministic 32-byte KEK) so cmd/api's `recwire.LocalPorts` builds non-nil `Ports`. The hex string is a fixed constant — `"ABCD"` × 16 = 64 chars = 32 bytes — committed verbatim so the harness is self-contained.
- `tests/smoke/seed.go` — `SeedTenantAndAdmin` now sets `kms_kek_id = "smoke-kek-default"` (replaces "smoke-kek-<orgCode>"). One-line change; documented in the existing comment block.
- `cmd/api/main.go` — wraps `recwire.LocalPorts(...)` call in a tiny indirection `buildRecordingPorts(cfg, logger)` that returns `smokeOverrideRecordingPorts` when non-nil, else falls through to `recwire.LocalPorts`. ~6 LoC change. Path-correction note: this is `cmd/api`, NOT `internal/`; the override is build-tagged isolated.

**Touched docs** (close-out only — Phase 4 of pipeline):
- `PROJECT_STATUS.md` — new milestone row v0.0.27, 🎯 NEXT pointer update.
- `docs/architecture/10-end-to-end-testing-gaps.md` — § "Phase 1 — `tests/smoke/`" gets a "Status: COMPLETED 2026-05-XX" badge.
- `docs/references/plan-21b-*.md` — § 6 "Production lessons" filled in.
- This plan's `## Amendments: none` → `## Amendments (post-execution)` as needed.

## Tasks

### Task 1 — Harness extensions (no scenario yet)

**Goal:** add the FIVE reusable helpers + the cmd/api recording-ports override seam + the `Stack.Reset` implementation. Each subsequent task imports these; nothing else changes.

**Files created:**
- `tests/smoke/wsclient.go`
- `tests/smoke/survey_seed.go`
- `tests/smoke/recording_seed.go`
- `tests/smoke/respondent_helpers.go`
- `tests/smoke/clock.go`
- `tests/smoke/audit_noop.go`
- `cmd/api/smoke_overrides.go` (`//go:build smoke`)
- `cmd/api/smoke_overrides_prod.go` (`//go:build !smoke`)

**Files modified:**
- `tests/smoke/stack.go` — `Reset(t *testing.T)` body + `PgPool() *postgres.Pool` accessor (constructs a `pkg/postgres.Pool` from `s.PostgresDSN` on first call; cached; cleanup-on-test-process-exit).
- `tests/smoke/config.go` — `WriteSmokeConfig` appends the `recording.local_keks` map.
- `tests/smoke/seed.go` — `SeedTenantAndAdmin` uses the deterministic `"smoke-kek-default"` id.
- `cmd/api/main.go` — `recwire.LocalPorts(...)` call wrapped in `buildRecordingPorts(cfg, logger)` indirection.

**TDD discipline (RED → GREEN → REFACTOR):**
- RED: `tests/smoke/harness_test.go` (NEW) declares one unit test per helper that the helper is callable + returns the documented shape — and fails because the helper doesn't exist. **Watch each fail.**
- GREEN: implement each helper minimally. Use `context7` for any unfamiliar API.
- REFACTOR: extract shared HTTP-client setup into one helper if duplication shows up.

**Test additions:**
- `tests/smoke/harness_test.go` (`//go:build smoke`) — six tiny self-tests:
  - `TestHarness_DialOperator_WrongTokenRejected` — boot cmd/api, dial WS without a valid token, expect close-code or non-101 response.
  - `TestHarness_MinimalValidSurveySchema` — schema bytes parse as JSON; validator accepts it (call `surveysapi.Validator.Validate` directly).
  - `TestHarness_BuildRecordingFixture_RoundTrip` — encrypt then decrypt the fixture with the same KMS; assert plaintext round-trips.
  - `TestHarness_BuildCSVImport_FormatMatches` — call `internal/crm/service/import.ParseCSV` (or the public surface) against the helper-built bytes; expect ≥1 valid row parsed.
  - `TestHarness_FutureClock_Returns_AddedDuration` — call `FutureClock(31*24*time.Hour)()` and assert it returns ≥ now + 30 days.
  - `TestHarness_Stack_Reset_TruncatesSeededRow` — seed a respondent, call Reset, assert `SELECT count(*) FROM respondents = 0`.

**Quality bar** (mandatory before reporting DONE):
- `make ci` green
- `go test -tags=smoke -race -count=1 -timeout=10m ./tests/smoke/... ./cmd/api/...` — green (includes existing Plan-21 scenarios as regression net)
- `gofmt -l .` empty
- `golangci-lint run ./...` zero issues
- `make grep-time-after` OK
- Commit message: `test(smoke): harness extensions for Phase-1b (WS client, survey/recording/CSV helpers, clock, Reset)`

**Subagent prompt requirements** (CLAUDE.md rule #3):
- Read `docs/references/plan-21b-phase-1b-smoke-scenarios.md` (§ 2.1, 2.2, 2.6, 2.7, 3, 4.8 mandatory).
- Use `context7` to verify `coder/websocket` API + `wsjson` package before writing `wsclient.go`.
- TDD: red-green-refactor; watch each helper test fail FIRST.
- Path-correction: `internal/crm/service/import.go` may have a different export shape than guessed — read it.
- Quality bar listed above.
- Commit at the end.

---

### Task 2 — `TestSmoke_SurveyCreatePreviewActivate`

**Goal:** end-to-end smoke proving the surveys module's HTTP transport + service + version store + advisory-lock activation flow work against a real cmd/api + Postgres.

**Scenario:**
1. Seed `tenant + admin` via `SeedTenantAndAdmin`.
2. Admin logs in → access JWT.
3. POST `/api/surveys` `{"code":"smoke-surv-1","name":"Smoke survey"}` → 201 `{id}`. Assert `id` is a valid UUID.
4. POST `/api/surveys/:id/versions` with `MinimalValidSurveySchema()` body → 201 `{version_id, semver}`. Assert `semver = "1.0"` (or "0.1.0" — verify from `internal/surveys/api/dto.go::SaveVersionResponse` BEFORE writing).
5. POST `/api/surveys/:id/versions/:version_id/activate` → 200. Assert no error envelope.
6. POST `/api/surveys/:id/preview/run` with a sample answer body → 200 + non-empty preview state. **(Operator+ endpoint — but we still hit it as admin; admin satisfies the role gate.)**
7. (Regression net) POST `/api/surveys/:id/versions` with a different schema body → 201 second version. POST `/api/surveys/:id/versions/:second_version_id/activate` → 200. Direct SQL: `SELECT count(*) FROM survey_versions WHERE survey_id = :id AND is_active = true` → exactly 1. The partial-unique constraint `survey_versions_active_one` prevents two-active.

**Files created:** `cmd/api/smoke_surveys_test.go`.

**Test additions:** the scenario itself.

**TDD discipline:** RED — write the scenario; expect each step to fail because the preceding step's response shape mismatches your guess. Fix one shape at a time. GREEN: scenario passes end-to-end.

**Quality bar:** same as Task 1. Plus: must not break the four Plan-21 scenarios.

**Subagent prompt requirements:**
- Read `docs/references/plan-21b-phase-1b-smoke-scenarios.md` (§ 2.2, 2.8, 4.10).
- Read `internal/surveys/api/dto.go` (request/response shapes) BEFORE writing the test. Do not guess JSON field names.
- TDD: watch each step fail FIRST (good test failures = clear shape mismatch errors).
- Quality bar listed above.
- Commit message: `test(smoke): TestSmoke_SurveyCreatePreviewActivate exercises surveys CRUD + activation`.

---

### Task 3 — `TestSmoke_AdminCreatesProjectAndImportsRespondents`

**Goal:** end-to-end smoke proving the crm module's import pipeline (HTTP multipart → asynq job → KMS-encrypt phone numbers → Postgres rows) works.

**Scenario:**
1. Seed `tenant + admin` (deterministic `smoke-kek-default` KEK ID; references § 4.5).
2. Admin login → JWT.
3. POST `/api/projects` `{"code":"smoke-imp","name":"Import smoke"}` → 201 `{id: pid}`.
4. Build CSV bytes with `BuildCSVImport([][]string{ {"+79001234567","Alice","30","male","M","Moscow"}, {"+79007654321","Bob","45","female","F","Saint Petersburg"} })`. Verify the column shape against `internal/crm/service/import.go` BEFORE writing.
5. POST `/api/projects/:pid/respondents/import?format=csv&filename=phones.csv` with multipart `file=<bytes>` → 202 `{job_id: jid}`.
6. Poll `GET /api/imports/:jid` until status is the terminal completed state (literal value from `dto.go::ImportStatusDTO`). Use `WaitForImportStatus(t, addr, jwt, jid, "completed")` with a 30 s deadline.
7. Direct SQL `SELECT count(*) FROM respondents WHERE tenant_id = $1` → 2 (both rows landed). Direct SQL also asserts `phone_encrypted IS NOT NULL AND phone_hash IS NOT NULL` (KMS path actually executed; phone_hash is HMAC-SHA256 of plaintext + pepper).
8. (Negative regression net) Same import request from a tenant-B JWT against tenant-A's project → 404 (RLS + RequireSameTenant guard). Two-row helper since we have both tenants seeded in TenantIsolation already — reuse the pattern.

**Files created:** `cmd/api/smoke_admin_import_test.go`.

**TDD discipline:** RED — the test fails because the CSV columns or `ImportStatusDTO` field names don't match your guess. Read the source, fix, re-run. GREEN: green end-to-end against a fully-booted cmd/api + asynq workers.

**Quality bar:** same. Plus: the import job MUST actually complete within 30 s on a warm container stack; flakes here block Plan close-out.

**Subagent prompt requirements:**
- Read references § 2.2, 4.3, 4.4, 4.5.
- READ `internal/crm/service/import.go` for the CSV format BEFORE writing `BuildCSVImport`. Don't guess columns.
- READ `internal/crm/transport/http/dto.go::ImportStatusDTO` (line ~172) BEFORE writing the polling helper.
- TDD: watch import-status polling fail because the literal "completed" might actually be "succeeded" or similar.
- Quality bar.
- Commit: `test(smoke): TestSmoke_AdminCreatesProjectAndImportsRespondents validates HTTP→asynq→PG→KMS import flow`.

---

### Task 4 — `TestSmoke_OperatorReadyAndStateBroadcast`

**Goal:** end-to-end smoke proving the dialer FSM + realtime WS broadcast pipeline. Catches NATS subject-pattern mismatch and locator-wiring drift between the dialer-side publisher and the WS-side subscriber.

**Scenario:**
1. Seed `tenant + admin + operator` (`SeedTenantAndAdmin` + `SeedOperator(t, stack, admin.TenantID, "op-login", "OpPass123!")`).
2. Operator login → JWT.
3. `DialOperator(ctx, t, httpAddr, operatorJWT)` → WS connection on `ws://addr/api/operator/ws?token=...`.
4. ReadEvent (5 s timeout) → assert the initial snapshot frame arrives (`{"type":"snapshot", ...}` — verify the literal type field by reading `internal/realtime/api/events.go` or `internal/dialer/.../ws_adapter.go`).
5. (Optional, if `start_shift` requires a project + survey assignment first) Seed `project` + assign operator + activate a default survey. **Verify whether `POST /api/sessions/start` requires upfront assignment by reading `internal/dialer/service/.../start_shift.go`.** If it does, do the assignment via REST first.
6. POST `/api/sessions/start` with operator JWT → 200 `{state: "ready"}` (verify shape from `internal/dialer/transport/http/dto.go`).
7. ReadEvent → assert next frame shows the `ready` state. Frame type is likely `state_change` or `fsm_event` — read source.
8. POST `/api/sessions/pause` → 200. ReadEvent → assert pause state propagated.
9. POST `/api/sessions/end` → 200. ReadEvent → state=`offline`.
10. Close WS gracefully (`c.Close(websocket.StatusNormalClosure, "")`).

**Files created:** `cmd/api/smoke_operator_ws_test.go`.

**TDD discipline:** RED — the WS dial fails because the subprotocol or query-string format is wrong. Iterate.

**Quality bar:** same. Plus: WS reads MUST have explicit per-call timeout — no infinite-block tests on CI. `goleak.VerifyTestMain` is inherited from `cmd/api/main_test.go`; the WS goroutines must drain on `c.Close` (a stuck WS reader leaks).

**Subagent prompt requirements:**
- Read references § 2.2, 2.9, 4.6.
- READ `internal/dialer/transport/http/ws_adapter.go` (or the realtime equivalent) for the subprotocol literal BEFORE writing `DialOperator`.
- READ `internal/realtime/api/events.go` for the event-type literal BEFORE asserting frame shape.
- READ `internal/dialer/service/.../start_shift.go` to confirm whether `POST /api/sessions/start` requires upfront project/survey assignment.
- TDD: watch WS-read fail because the field name or value drifted.
- Quality bar.
- Commit: `test(smoke): TestSmoke_OperatorReadyAndStateBroadcast validates WS broadcast on FSM transitions`.

---

### Task 5 — `TestSmoke_RecordingSearchAndStream`

**Goal:** end-to-end smoke proving the recording module's search + stream + integrity-sha256 pipeline.

**Scenario:**
1. **Before bootAPI**: build a shared `*recwire.Ports` (smoke harness owns the same `LocalObjectStore` + `LocalDEKUnwrapper` instance cmd/api will use). Call `SetSmokeRecordingPorts(ports)` BEFORE `bootAPI(t, stack)`.
2. Seed `tenant + admin + project + call`.
3. `fixture := BuildRecordingFixture(t, stack, tenantA.TenantID, callA)` — encrypts a deterministic 32 kB plaintext blob (e.g. `bytes.Repeat([]byte("opus-fixture-block"), 2048)`) via the smoke KMS, yields `{Ciphertext, SHA256Hex, WrappedDEKHex, KMSKeyID, Plaintext, Bucket, Key}`.
4. `SeedRecording(t, stack, tenantA.TenantID, callA, fixture)` — INSERT `call_recordings` row + `ports.Objects.Put(bucket, key, ciphertext)`.
5. Admin login → JWT.
6. GET `/api/recordings/search` → 200 + 1-item page. Assert returned `sha256` field equals `fixture.SHA256Hex` (ciphertext sha, per ADR-0005). Assert tenant scoping (cross-tenant B JWT returns empty page).
7. GET `/api/calls/:callA/recording` with admin JWT → 200 + response body is the plaintext (sha256 of body equals sha256 of `fixture.Plaintext`).
8. GET `/api/calls/:callA/recording` with `Range: bytes=0-1023` → 416 Range Not Satisfiable (ADR-0005 §15.4 contract).
9. (Cross-tenant regression net) GET `/api/calls/:callA/recording` with tenant-B JWT → 404 (RLS).

**Files created:** `cmd/api/smoke_recording_test.go`.

**TDD discipline:** RED — the test fails because the `SetSmokeRecordingPorts` seam isn't yet wired (`smokeOverrideRecordingPorts` field undefined) OR because the encryption format doesn't match what the recording service expects (wrong AAD shape, wrong wrap format). Iterate by reading `internal/recording/service/verify.go` + `internal/recording/crypto/*.go` for the canonical envelope format.

**Quality bar:** same. Plus: encryption fixture must use `BuildAAD(tenantID, "recording.dek", callID)` per Plan 13.2.5 v2 envelope contract (see `internal/recording/crypto/envelope.go` or similar).

**Subagent prompt requirements:**
- Read references § 2.2, 2.6, 2.7, 4.7, plus § 1 ADR-0005.
- READ `internal/recording/crypto/*.go` for the canonical envelope format (`BuildAAD` shape, wrap algorithm, length prefixes). Do NOT reinvent.
- READ `internal/recording/transport/http/*.go` for the search response shape + stream content-type + Range-rejection handling.
- TDD: watch decryption fail first (wrong AAD or wrong format). Iterate on the fixture builder until decrypt is clean.
- Quality bar.
- Commit: `test(smoke): TestSmoke_RecordingSearchAndStream validates recording HTTP + KMS + integrity sha256`.

---

### Task 6 — `TestSmoke_RespondentSoftDelete152FZ`

**Goal:** end-to-end smoke proving the 152-ФЗ deletion-right pipeline: HTTP soft-delete → 30-day grace → PurgeWorker hard-deletes.

**Scenario:**
1. Seed `tenant + admin + project + respondent` (use the import helper from Task 3 to insert one respondent, OR a direct SQL seed — pick whichever is shorter; direct SQL is fine since import flow is already covered by Task 3).
2. Admin login → JWT.
3. DELETE `/api/respondents/:rid` → 204 (or 200; verify from `respondent_handler.go::deleteRespondent`). Direct SQL: `SELECT deleted_at FROM respondents WHERE id = :rid` → non-null.
4. (Pre-purge regression net) Direct SQL: row STILL exists (`SELECT count(*) WHERE id = :rid` → 1).
5. Build a `crmservice.PurgeWorker` in-test:
   ```go
   import (
       crmservice "github.com/sociopulse/platform/internal/crm/service"
   )

   pool := stack.PgPool()        // accessor from Task 1
   store := crmstore.NewRespondentStore(pool, …)   // build from same pool + tenancy deps
   audit := smoke.NoopAuditLogger{}
   clock := smoke.FutureClock(31 * 24 * time.Hour) // i.e. cutoff = now() + 1d
   worker := crmservice.NewPurgeWorker(pool, store, audit, 30*24*time.Hour, 1000, clock)
   require.NoError(t, worker.Run(ctx))
   ```
   Verified by: `internal/crm/service/purge.go:66-101` — the constructor accepts these exact types.
6. Direct SQL: `SELECT count(*) FROM respondents WHERE id = :rid` → 0. Row is physically gone.
7. (Idempotency regression) Call `worker.Run(ctx)` AGAIN → no error. Row still gone (no resurrection bug).

**Files created:** `cmd/api/smoke_purge_test.go`.

**TDD discipline:** RED — the test fails because direct construction of `RespondentStore` is wrong (wrong constructor signature, wrong deps shape). Iterate by reading the canonical PurgeWorker test (`internal/crm/service/purge_test.go`) for the canonical fake or by reading `crmstore.NewRespondentStore` for production wiring.

**Quality bar:** same. Plus: scenario MUST complete in under 5 s (no actual `time.Sleep`s; clock injection makes this instant).

**Subagent prompt requirements:**
- Read references § 2.4, 4.2.
- READ `internal/crm/service/purge_test.go` for the canonical construction pattern.
- READ `internal/crm/store/respondent_store.go::NewRespondentStore` (or wherever it's defined) for the production constructor signature.
- TDD: watch the in-test construction fail first because a dep field doesn't match.
- Quality bar.
- Commit: `test(smoke): TestSmoke_RespondentSoftDelete152FZ validates HTTP soft-delete → PurgeWorker hard-delete`.

---

## Pre-commit gate (canonical — applies to EVERY task)

Per CLAUDE.md workflow rule #3 + `09-agent-workflow-improvements.md`:

```bash
make ci                                                           # lint + vet + grep-time-after + test
go test -tags=smoke -race -count=1 -timeout=15m ./tests/smoke/... ./cmd/api/...  # smoke regression net
gofmt -l .                                                        # must be empty
make build                                                        # all binaries compile
```

`make ci` is canonical for production code (unit + lint). The smoke step is in addition because Plan 21b is a smoke-test-only plan — the smoke suite IS the change. The smoke-tagged suite must regress-test the four Plan-21 scenarios on every commit; Task 1's `harness_test.go` adds six more.

If `gopls` shouts but these commands are green → cache pollution; ignore and commit (Plan-21 retro confirmed this).

## Re-review proportionality (per task)

Per `09-agent-workflow-improvements.md` #7:

| Diff scope | Action |
|---|---|
| Test-only diff ≤ 30 lines, no new public symbols | Single re-review (spec only) |
| Test-only diff > 30 lines OR touches harness helper | Full 2-stage re-review |
| Production code change (Task 1's `cmd/api/main.go` indirection + `cmd/api/smoke_overrides_prod.go`) | Full 2-stage re-review on the original diff; subsequent test-only commits get the lighter loop |

Task 1's production-code touch is **minimal** (one helper function + a build-tag stub-vs-real pair). It still warrants full 2-stage review because it's the production code path going through smoke-tagged seams; future production refactors must not silently break this.

## Self-review (plan-time checklist, before dispatching Task 1)

- [x] Every cross-boundary assertion in Context has `Verified by:` citation or `Assumed (not verified)` marker.
- [x] Every task references concrete files with concrete lines (route mounts, schema columns, dto shapes).
- [x] No placeholders ("TODO", "fill in", "appropriate error handling", "similar to Task N").
- [x] Type/signature consistency: helpers defined in Task 1 are used coherently in Tasks 2-6 with matching shapes.
- [x] Pre-commit gate explicitly written into each task.
- [x] Plan vocabulary checked against `CONTEXT.md` — Operator, Respondent, FSM, Recording, RLS, 152-ФЗ all from glossary.
- [x] No contradiction with existing ADRs (ADR-0005 Range-rejection contract is reinforced by scenario 5, not contradicted; ADR-0015 TDD is followed).
- [x] `docs/references/plan-21b-*.md` exists and is referenced in this plan's header + every task's subagent prompt requirements.
- [x] Paths in the File-structure section match actual scaffolding (`tests/smoke/`, `cmd/api/`; NO `internal/X` references that should be `pkg/X`).

## Task summary

| Task | Goal | Files (new / modified) | Diff size (estimated) |
|---|---|---|---|
| 1 | Harness extensions | 8 new + 4 modified | ~600 LoC |
| 2 | Survey CRUD + activate | 1 new | ~200 LoC test |
| 3 | Admin import (HTTP→asynq→KMS→PG) | 1 new | ~250 LoC test |
| 4 | Operator WS + FSM state broadcast | 1 new | ~250 LoC test |
| 5 | Recording search + stream + sha256 + Range | 1 new | ~300 LoC test |
| 6 | 152-ФЗ soft-delete + PurgeWorker | 1 new | ~200 LoC test |

Total: ~1800 LoC across 14 new + 4 modified files. Five new HTTP-surface scenarios; closes Phase 1 of `docs/architecture/10-end-to-end-testing-gaps.md`.

## Tag

On close-out: `v0.0.27-phase-1b-smoke-scenarios`.

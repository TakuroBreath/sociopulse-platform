# Plan 22 — REST collection (Bruno) for the public HTTP surface

> Plan ID: 22
> References: [`docs/references/plan-22-rest-collection.md`](../../references/plan-22-rest-collection.md) (**MUST be read by every implementer subagent BEFORE writing files**)
> Closure plan: [`docs/architecture/10-end-to-end-testing-gaps.md`](../../architecture/10-end-to-end-testing-gaps.md) § "Phase 2 — REST collection" — Plan 22 closes scenario B from § "What we do not test today".
> Related: Plan 21 (`v0.0.26-e2e-smoke-foundation`) + Plan 21b (`v0.0.27-phase-1b-smoke-scenarios`) — closed Phase 1 of the same closure plan. Smoke = automated cross-module regression net; REST collection = human-driven exploration of the same surface.
> Affected ADRs: none (deliverable is a documentation artefact, not a code/architectural change). The collection MIRRORS existing API contracts; it does not introduce new ones.

## Amendments: none

## Goal

Ship a Bruno collection under `docs/api/collections/sociopulse/` covering every public HTTP endpoint of cmd/api, organised by module, with login flow + JWT auto-capture + happy-path + at least one error case per endpoint. The collection is for:

- **Developer manual exploration** — open the project in Bruno UI, edit env vars, click through requests.
- **QA pre-release sweep** — run `bru run --env smoke` against the smoke testcontainer harness for a smoke-level confidence check before tag.
- **Onboarding a new agent** — the collection enumerates the public surface in a discoverable, executable form (more pragmatic than an OpenAPI inventory split across 3 modules).

**Success criteria** (mirrors `10-end-to-end-testing-gaps.md` § Phase 2):
- ≥ 1 `.bru` file per public HTTP endpoint, organised in module subfolders under `docs/api/collections/sociopulse/`.
- `environments/{smoke,dev}.bru` with `{{base_url}}` + auth defaults.
- Login flow at the top of the auth folder; `script:post-response` persists `access_token` + `refresh_token` env vars.
- Authenticated requests use `auth:bearer { token: {{access_token}} }`.
- For at least the canonical security-boundary endpoints (auth refresh, cross-tenant project GET, RBAC-gated POST), ship a paired error-case `.bru` covering 401 / 403 / 404 / 400.
- README.md in the collection root explains: how to open in Bruno UI, how to run via CLI, how to extend, the WS endpoints not covered.
- Vocabulary in `meta.name` + folder names strictly from `CONTEXT.md`.

**Non-goals:**
- CI integration (`bru run` as a CI job). Deferred — Plan 22 ships the collection; a future plan decides whether to add a `rest` job + whether it gates tag-push.
- WebSocket coverage (`/api/operator/ws`, `/api/realtime/ws`). Bruno is HTTP-only; smoke `tests/smoke/wsclient.go` is the canonical WS test surface. Document the gap in README.
- Production environment file. Bruno collection is for dev/test, not prod ops.
- New OpenAPI generation. The collection mirrors existing `internal/<module>/transport/http/dto.go` shapes + the per-module OpenAPI files where they exist (`docs/api/{billing,recording,reports}/`).

## Context (reality-checked 2026-05-16)

### Existing state (verified, with citation)

- **`docs/api/` directory exists** with subdirs `billing/`, `recording/`, `reports/` (OpenAPI specs from earlier plans) + a top-level `README.md`. `collections/` does NOT exist yet — Plan 22 creates it.
  Verified by: `ls -la docs/api/`.

- **8 HTTP transport modules** ship public endpoints: `auth`, `crm`, `surveys`, `dialer`, `recording`, `realtime`, `billing`, `reports`. Total ~65-70 endpoints — full inventory in references file § 3.
  Verified by: `find internal -name routes.go -path '*/transport/http/*'` + per-module grep.

- **Plan 21b wire-format reality is authoritative for request/response shapes.** Lessons 10-15 in `docs/references/plan-21b-phase-1b-smoke-scenarios.md` § 6 capture the field-name / status-code corrections that the smoke scenarios verified empirically. Plan 22 mirrors these; mismatches mean the .bru is wrong.
  Verified by: `docs/references/plan-21b-phase-1b-smoke-scenarios.md:lessons-10-15`.

- **Bruno's `.bru` markup language is plain-text JSON-adjacent.** Per-request file format: `meta { name, type, seq }` + HTTP method block (`get`, `post`, `patch`, `delete`) with `url` + `body` + `auth` + optional `headers`, `body:json` / `body:multipart-form`, `auth:bearer`, `script:pre-request` / `script:post-response` (JavaScript via `bru.setEnvVar`/`bru.getVar`), `tests` (chai-style assertions), `assert` (declarative). CLI: `bru run --env <name>` executes the collection and exits non-zero on test failure.
  Verified by: `context7` against `/usebruno/bruno-docs` (queried 2026-05-16).

- **The `make dev-up` stack** binds cmd/api on `http://127.0.0.1:8080` by default (`configs/development/config.yaml`). The `dev` Bruno environment defaults to this URL.
  Verified by: `configs/development/config.yaml` + `Makefile::dev-up`.

- **The smoke testcontainer harness uses ephemeral 127.0.0.1:N ports per scenario.** No stable URL — Bruno cannot point at it. The `smoke` Bruno environment is for manual operator use against a fresh `make dev-up` + a seeded tenant (NOT against the automated smoke scenarios).
  Verified by: `tests/smoke/ports.go::PickFreeAddr` + `cmd/api/smoke_test.go::bootAPI`.

### File structure (created / modified)

**Created** (collection root + 4 subdirs + files; estimate ~80 files total):

- `docs/api/collections/sociopulse/bruno.json` — collection-level config (id, name, type=collection, version)
- `docs/api/collections/sociopulse/environments/smoke.bru` — env for `make dev-up`
- `docs/api/collections/sociopulse/environments/dev.bru` — same defaults; copy of smoke (placeholder for future divergence)
- `docs/api/collections/sociopulse/README.md` — how-to-use; WS coverage note; CI integration as deferred
- `docs/api/collections/sociopulse/fixtures/respondents.csv` — minimal CSV for `POST /api/projects/:id/respondents/import` multipart upload (mirrors `BuildCSVImport` from `tests/smoke/respondent_helpers.go`)
- `docs/api/collections/sociopulse/auth/{01_login,02_refresh,03_logout,me,me_password,totp_enroll,totp_confirm,totp_disable,totp_status}.bru` (9 files)
- `docs/api/collections/sociopulse/auth/admin/{create_user,list_users,get_user,update_roles,archive_user,restore_user,reset_password}.bru` (7 files)
- `docs/api/collections/sociopulse/auth/_errors/{login_wrong_password,refresh_after_logout}.bru` (2 files — canonical security regressions)
- `docs/api/collections/sociopulse/crm/projects/{list,get,create,update,pause,resume,archive,assign,unassign,progress,members}.bru` (11 files)
- `docs/api/collections/sociopulse/crm/respondents/{create,import,search,get,get_with_phone,delete}.bru` (6 files)
- `docs/api/collections/sociopulse/crm/imports/get_status.bru` (1 file)
- `docs/api/collections/sociopulse/crm/_errors/{cross_tenant_project_get,operator_creates_project_403}.bru` (2 files)
- `docs/api/collections/sociopulse/surveys/{list,get,versions_list,versions_active,preview_run,validate}.bru` (6 files)
- `docs/api/collections/sociopulse/surveys/admin/{create,update,archive,save_version,activate_version}.bru` (5 files)
- `docs/api/collections/sociopulse/surveys/_errors/{cross_tenant_survey_get,activate_nonexistent_404}.bru` (2 files)
- `docs/api/collections/sociopulse/dialer/sessions/{start,end,pause,resume,me}.bru` (5 files)
- `docs/api/collections/sociopulse/dialer/calls/{submit_status,hangup}.bru` (2 files)
- `docs/api/collections/sociopulse/dialer/verify/{start,done}.bru` (2 files)
- `docs/api/collections/sociopulse/dialer/operator/force.bru` (1 file)
- `docs/api/collections/sociopulse/dialer/_errors/{pause_without_reason_400,cross_tenant_hangup_404}.bru` (2 files)
- `docs/api/collections/sociopulse/recording/{search,get_audio,verify_checksum}.bru` (3 files)
- `docs/api/collections/sociopulse/recording/_errors/{range_request_416,cross_tenant_stream_404}.bru` (2 files)
- `docs/api/collections/sociopulse/billing/finance/{dashboard,projects,breakdown,byMonth}.bru` (4 files)
- `docs/api/collections/sociopulse/billing/tariffs/{get,patch}.bru` (2 files)
- `docs/api/collections/sociopulse/billing/_errors/operator_patches_tariff_403.bru` (1 file)
- `docs/api/collections/sociopulse/reports/{list_kinds,export,custom}.bru` (3 files)
- `docs/api/collections/sociopulse/reports/jobs/{get,download}.bru` (2 files)
- `docs/api/collections/sociopulse/reports/_errors/cross_tenant_job_404.bru` (1 file)

Total: ~80 files. Average ~25 LoC per `.bru` file. Estimated deliverable: ~2000 LoC.

**Modified** (1 file):
- `docs/api/README.md` — append a section pointing at `collections/sociopulse/` (the existing README lists the per-module OpenAPI subdirs; add the new collection sibling).

**Touched in close-out** (Phase 4 only):
- `PROJECT_STATUS.md` — new milestone row v0.0.28 + headline summary update.
- `docs/architecture/10-end-to-end-testing-gaps.md` — § "Phase 2" status badge → COMPLETE.
- `docs/architecture/04-testing-strategy.md` — note REST collection alongside the smoke layer (1-paragraph addition).
- `docs/references/plan-22-rest-collection.md` § 6 "Production lessons" — filled per CLAUDE.md rule #8.
- This plan's `## Amendments: none` → `## Amendments (post-execution)` as needed.

## Tasks

### Task 1 — Collection scaffold + auth flow + foundation

**Goal:** the scaffold a future contributor needs to extend the collection — `bruno.json`, environments, README, the canonical login flow with JWT capture, the full auth module (16 endpoints incl. admin + 2 negative cases), and the multipart-CSV fixture.

**Files created:**
- `docs/api/collections/sociopulse/bruno.json`
- `docs/api/collections/sociopulse/environments/smoke.bru`
- `docs/api/collections/sociopulse/environments/dev.bru`
- `docs/api/collections/sociopulse/README.md`
- `docs/api/collections/sociopulse/fixtures/respondents.csv`
- `docs/api/collections/sociopulse/auth/` — 9 user-side .bru files + 7 admin .bru files + 2 error .bru files (18 total)

**Files modified:**
- `docs/api/README.md` — append collection pointer (1-paragraph addition)

**Verification:**
- `bru run --env dev --filename docs/api/collections/sociopulse/auth/01_login.bru` succeeds against `make dev-up` + a seeded tenant (`SMOKE-DEFAULT` org_code, admin user `alice`/`AlicePass123!`); writes `access_token` to env (`environments/dev.bru` after run shows the captured value, OR an explicit `tests` assertion confirms the env var is set).
- The `_errors/login_wrong_password.bru` request returns 401 + the `auth.token_invalid` error envelope; test block asserts both.
- The README documents: (a) how to install Bruno (UI + CLI), (b) how to seed a tenant for first-time use, (c) how to run the canonical login flow, (d) the WS endpoints not covered + pointer at `tests/smoke/wsclient.go`, (e) CI integration as a future plan.

**TDD discipline:**
- RED: write the login `.bru` first; run via `bru run`; expect failure (env var not set, response shape mismatch). Iterate.
- GREEN: green when login succeeds + access_token persists.
- REFACTOR: extract the post-response token-capture pattern as a reusable snippet referenced in README ("every login-like flow uses this script:post-response").

**Quality bar:**
- Bruno CLI installed locally (`npm install -g @usebruno/cli` if not already).
- `bru run --env dev` for every .bru in `auth/` exits 0.
- `make ci` still green (collection is doc-only — no Go code changed; `make ci` should be unaffected). Verify by running.
- `gofmt -l .` empty (no Go code touched — verify the touched docs don't break anything via accidental whitespace).
- Commit: `docs(api): Plan 22 Task 1 — Bruno collection scaffold + auth flow + fixtures`.

**Subagent prompt requirements** (CLAUDE.md rule #3):
- Read `docs/references/plan-22-rest-collection.md` (sections 1, 2, 3-auth, 4 mandatory).
- Use `context7` to verify Bruno `.bru` syntax + CLI flags BEFORE writing files (especially `body:multipart-form` for the fixture upload — the syntax is non-obvious).
- Read `internal/auth/transport/http/dto.go` + `routes.go` for EVERY auth endpoint's request/response shape — NO guessing.
- Vocabulary from `CONTEXT.md`.
- Commit at end.

---

### Task 2 — CRM + surveys collection

**Goal:** ship the full CRM (project + respondent + import) + surveys collection, including the multipart import using the Task 1 fixture, plus 4 canonical error cases (cross-tenant + RBAC).

**Files created:**
- `docs/api/collections/sociopulse/crm/projects/` — 11 files
- `docs/api/collections/sociopulse/crm/respondents/` — 6 files
- `docs/api/collections/sociopulse/crm/imports/get_status.bru` — 1 file
- `docs/api/collections/sociopulse/crm/_errors/` — 2 files
- `docs/api/collections/sociopulse/surveys/` — 6 user-side files + 5 admin files + 2 error files (13 total)

**Verification:**
- For each .bru: `bru run --env dev --filename <path>` exits 0 on a `make dev-up` stack with a seeded tenant + admin user.
- The `crm/respondents/import.bru` references `fixtures/respondents.csv` via Bruno's multipart-form `file: @fixtures/respondents.csv` syntax (verify exact syntax via context7); response 202 + `job_id` captured to env; chained `crm/imports/get_status.bru` polls until `state == "succeeded"`.
- The cross-tenant error cases (`_errors/cross_tenant_project_get.bru`, `surveys/_errors/cross_tenant_survey_get.bru`) override `{{access_token}}` with a different-tenant JWT (env var `{{other_tenant_token}}`) — the test asserts 404 + empty body OR `recording.not_found`-style envelope.
- The RBAC error case (`_errors/operator_creates_project_403.bru`) uses `{{operator_token}}` env var — test asserts 403.

**TDD discipline:** RED — each .bru fails first because field name / status code guessed wrong. Read source. Fix. GREEN.

**Quality bar:** same as Task 1. Specifically: `bru run --env dev` for every new .bru exits 0; happy-path + error-case both green.

**Subagent prompt requirements:** read references file § 3-crm + § 3-surveys; read `internal/crm/transport/http/dto.go` + `internal/surveys/transport/http/dto.go` for exact field names; verify Bruno multipart-form syntax via context7; commit at end.

---

### Task 3 — Dialer + recording + realtime collection

**Goal:** operator-path endpoints (dialer sessions + calls + verify) + recording (search/stream/verify) + the WS documentation note in README.

**Files created:**
- `docs/api/collections/sociopulse/dialer/sessions/` — 5 files
- `docs/api/collections/sociopulse/dialer/calls/` — 2 files
- `docs/api/collections/sociopulse/dialer/verify/` — 2 files
- `docs/api/collections/sociopulse/dialer/operator/force.bru` — 1 file
- `docs/api/collections/sociopulse/dialer/_errors/` — 2 files
- `docs/api/collections/sociopulse/recording/` — 3 files
- `docs/api/collections/sociopulse/recording/_errors/` — 2 files

**Files modified:**
- `docs/api/collections/sociopulse/README.md` — flesh out the "WebSocket coverage" section (added skeleton in Task 1) with explicit pointers at `tests/smoke/wsclient.go` + `internal/dialer/transport/http/ws.go`.

**Verification:**
- The dialer scenario chain works: `sessions/start.bru` (requires `{{project_id}}` env var seeded by Task 2's project-create flow OR by a Task 3 `pre-request` script) → `sessions/me.bru` returns state=`ready` → `sessions/pause.bru` with `{reason: "lunch"}` → state=`pause` → `sessions/resume.bru` → state=`ready` → `sessions/end.bru` → state=`offline`.
- `dialer/_errors/pause_without_reason_400.bru` asserts 400 + validation error message.
- `dialer/_errors/cross_tenant_hangup_404.bru` asserts 404 (Plan 21 + Plan 21b regression net).
- The recording flow: `recording/search.bru` returns a page (may be empty if no recordings seeded — assert 200 + items array, NOT non-empty); `recording/get_audio.bru` requires a known `:id` (assert 200 + content-type `audio/ogg` if seeded; OR 404 if not seeded with `{{recording_call_id}}`); `recording/_errors/range_request_416.bru` sends `Range: bytes=0-1023` → asserts 416 + `recording.range_not_satisfiable`.

**TDD discipline:** RED first.

**Quality bar:** same.

**Subagent prompt requirements:** references file § 3-dialer + § 3-recording + § 4.3 (no WS coverage) + Plan-21b lesson 12 (dialer wire-format reality); commit at end.

---

### Task 4 — Billing + reports collection + final polish

**Goal:** finance + tariffs + reports endpoints; refresh README; final pass to ensure every endpoint has a .bru + every canonical error case is present.

**Files created:**
- `docs/api/collections/sociopulse/billing/finance/` — 4 files
- `docs/api/collections/sociopulse/billing/tariffs/` — 2 files
- `docs/api/collections/sociopulse/billing/_errors/operator_patches_tariff_403.bru` — 1 file
- `docs/api/collections/sociopulse/reports/` — 3 user-side files
- `docs/api/collections/sociopulse/reports/jobs/` — 2 files
- `docs/api/collections/sociopulse/reports/_errors/cross_tenant_job_404.bru` — 1 file

**Files modified:**
- `docs/api/collections/sociopulse/README.md` — final polish: usage examples, common gotchas (env-var persistence, multipart syntax, error envelope shape), pointer at `docs/api/{billing,recording,reports}/` for the underlying OpenAPI specs.

**Verification:**
- `bru run --env dev` against every .bru in `billing/` + `reports/` exits 0.
- The tariff PATCH flow: `tariffs/get.bru` returns `{version: N, ...}`; `tariffs/patch.bru` increments version (via `script:post-response` capturing the new version) and the follow-up `tariffs/get.bru` re-fetch shows the new version.
- The async report flow: `reports/export.bru` with a large `period` returns 202 + `jobID` (captured to env); `reports/jobs/get.bru` polls until `state == "succeeded"`; `reports/jobs/download.bru` returns a 24h presigned-URL S3 redirect (302 OR a direct URL in JSON — read the handler to confirm).
- The 403 case `billing/_errors/operator_patches_tariff_403.bru` asserts 403.
- The cross-tenant case `reports/_errors/cross_tenant_job_404.bru` asserts 404 (reports' `jobIDTenantGuard` per Plan 13.3 — verify the exact envelope code).

**Final sanity sweep:**
- `find docs/api/collections/sociopulse/ -name '*.bru' | wc -l` ≥ 75 (~80 expected).
- For every public endpoint in references file § 3 tables, there exists a corresponding .bru.
- The collection opens cleanly in Bruno UI (no syntax errors).
- The README is complete and self-contained.

**TDD discipline:** RED first.

**Quality bar:** same.

**Subagent prompt requirements:** references file § 3-billing + § 3-reports + the existing `docs/api/billing/v1/openapi.yaml` (canonical billing spec); commit at end.

## Pre-commit gate (canonical — applies to EVERY task)

The collection is documentation, NOT Go code, so the project's standard gate is mostly unchanged:

```bash
make ci                                                                            # lint + vet + grep-time-after + test (touches Go; collection should not break it)
gofmt -l .                                                                         # empty (no Go changes — guards against accidental edits)
make build                                                                         # all binaries still compile
```

PLUS collection-specific:

```bash
# Bruno CLI must be installed: npm install -g @usebruno/cli (or local dev container)
cd docs/api/collections/sociopulse
bru run --env dev                                                                  # all .bru in the collection pass against `make dev-up`
```

If `make dev-up` isn't available to the implementer (e.g. CI), document this — the collection ships, the CI integration is a future plan.

## Re-review proportionality (per task)

This is a doc-deliverable plan, not a code change. Per `09-agent-workflow-improvements.md` #7:

| Diff scope | Action |
|---|---|
| Documentation-only diff (.bru + .md + .json) | **Single re-review (spec compliance only)** — each task's .bru files are mechanical (same shape, different endpoint). Spec review confirms every endpoint has a .bru + every claim matches dto.go. Code-quality review would have nothing to assess. |
| Edge: if Task 1's README or `bruno.json` contains a substantive structural decision (folder layout, env-var naming convention, fixture format) | Single re-review of THAT file (other files inherit the convention) |

No full 2-stage review needed for any task. The smoke layer (Plans 21+21b) is the executable regression net; this collection is the manual exploration surface.

## Self-review (plan-time checklist, before dispatching Task 1)

- [x] Every cross-boundary assertion in Context has `Verified by:` citation.
- [x] Every task references concrete files with concrete contents (`auth/01_login.bru`, etc.).
- [x] No placeholders ("TODO", "fill in", "appropriate handling").
- [x] Type consistency: env-var names (`access_token`, `refresh_token`, `tenant_id`, `org_code`, `project_id`, etc.) are used coherently across Tasks 1–4.
- [x] Pre-commit gate explicitly written into each task.
- [x] Plan vocabulary checked against `CONTEXT.md` (Tenant, Operator, Respondent, Project, Survey, Recording, RLS, 152-ФЗ).
- [x] No contradiction with existing ADRs (collection mirrors existing API contracts; no new architectural decisions).
- [x] `docs/references/plan-22-rest-collection.md` exists and is referenced in this plan's header + every task's subagent prompt requirements.
- [x] Paths match actual scaffolding (`docs/api/collections/sociopulse/` is new; collection root mirrors per-module subfolders).

## Task summary

| Task | Goal | Files (new / modified) | Diff size (estimated) |
|---|---|---|---|
| 1 | Scaffold + env + auth (16) + 2 errors + fixtures + README | 1 bruno.json + 2 envs + 18 .bru + 1 README + 1 .csv = 23 new + 1 modified | ~600 LoC |
| 2 | CRM (18) + 2 errors + surveys (11) + 2 errors | 33 new files | ~700 LoC |
| 3 | Dialer (10) + 2 errors + recording (3) + 2 errors + README update | 17 new + 1 modified | ~450 LoC |
| 4 | Billing (6) + 1 error + reports (5) + 1 error + README polish | 13 new + 1 modified | ~350 LoC |

Total: ~86 new files + 1 modified, ~2100 LoC across 4 tasks. Closes Phase 2 of `docs/architecture/10-end-to-end-testing-gaps.md`.

## Tag

On close-out: `v0.0.28-rest-collection`.

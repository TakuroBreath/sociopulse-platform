# Plan 22 — REST collection (Bruno) for the public HTTP surface

> **Subagents must read this file BEFORE writing code.** Captures the
> canonical specs, the verified endpoint inventory, and the Bruno
> conventions a future agent needs to extend the collection without
> rediscovering them. The plan file at
> `docs/superpowers/plans/2026-05-16-22-rest-collection.md` tells you
> WHAT to write; this file tells you WHERE the contracts live.

## 1. Canonical specs

- **Closure plan (the master design):** [`docs/architecture/10-end-to-end-testing-gaps.md`](../architecture/10-end-to-end-testing-gaps.md)
  - § "Phase 2 — REST collection (target: ~4 hours)" — the original scoping. Bruno is the chosen tool (alternatives `.postman_collection.json` / Hurl / HTTPie mentioned for record).
  - Targets per the closure plan: (a) all public endpoints grouped by module; (b) login flow at the top, JWT captured into env automatically; (c) happy-path + at least one error case per endpoint; (d) usage = developer manual exploration, QA pre-release sweep, onboarding a new agent on the system. Phase 2 closes scenario B from § "What we do not test today".
- **Testing strategy:** [`docs/architecture/04-testing-strategy.md`](../architecture/04-testing-strategy.md) § "What this strategy DOES cover at the system level" — REST collection complements the smoke layer; smoke = automated cross-module regression net, REST collection = human-driven exploration of the same surface.
- **Plan-21b production lessons:** [`docs/references/plan-21b-phase-1b-smoke-scenarios.md`](plan-21b-phase-1b-smoke-scenarios.md) § 6 — the wire-format reality discovered during Plan 21b (lessons 10–15) is the authoritative source for request/response shapes. Plan 22 MUST mirror it; if a `.bru` file disagrees with a Plan-21b lesson, the .bru is wrong.
- **Per-module OpenAPI** (where it exists):
  - `docs/api/billing/v1/openapi.yaml` — billing endpoints
  - `docs/api/recording/` — recording endpoints
  - `docs/api/reports/` — reports endpoints
  - Other modules (auth, crm, surveys, dialer, realtime) currently have NO standalone OpenAPI doc; the `internal/<module>/transport/http/dto.go` files are the wire contract source.
- **Domain glossary:** [`CONTEXT.md`](../../CONTEXT.md) — vocabulary canon. Collection names + descriptions use glossary terms (Tenant, Operator, Respondent, Survey, Project, Recording, RLS, 152-ФЗ).

## 2. Bruno format basics (verified via `context7` against `/usebruno/bruno-docs`)

### File layout

```
docs/api/collections/sociopulse/
├── bruno.json                          # collection-level config (id, name, type=collection, version)
├── environments/
│   ├── smoke.bru                       # local testcontainer stack DSNs
│   └── dev.bru                         # docker-compose dev stack defaults
├── auth/
│   ├── 01_login.bru                    # canonical first request; populates access_token + refresh_token env vars
│   ├── 02_refresh.bru
│   ├── 03_logout.bru
│   ├── me.bru
│   ├── me_password.bru
│   ├── totp_enroll.bru
│   ├── totp_confirm.bru
│   ├── totp_disable.bru
│   ├── totp_status.bru
│   └── admin/
│       ├── create_user.bru
│       ├── list_users.bru
│       ├── get_user.bru
│       ├── update_roles.bru
│       ├── archive_user.bru
│       ├── restore_user.bru
│       └── reset_password.bru
├── crm/
│   ├── projects/{list,get,create,update,pause,resume,archive,assign,unassign,progress,members}.bru
│   ├── respondents/{create,import,search,get,get_with_phone,delete}.bru
│   └── imports/get_status.bru
├── surveys/
│   ├── {list,get,versions_list,versions_active,preview_run,validate}.bru
│   └── admin/{create,update,archive,save_version,activate_version}.bru
├── dialer/
│   ├── sessions/{start,end,pause,resume,me}.bru
│   ├── calls/{submit_status,hangup}.bru
│   ├── verify/{start,done}.bru
│   └── operator/force.bru
├── recording/
│   ├── search.bru
│   ├── get_audio.bru
│   └── verify_checksum.bru
├── billing/
│   ├── finance/{dashboard,projects,breakdown,byMonth}.bru
│   └── tariffs/{get,patch}.bru
├── reports/
│   ├── {list_kinds,export,custom}.bru
│   └── jobs/{get,download}.bru
└── README.md                           # how to use, how to extend
```

WebSocket endpoints (`/api/operator/ws`, `/api/realtime/ws`) are **NOT** covered by Bruno (it's HTTP-only). Note them in README as "use smoke `tests/smoke/wsclient.go` for WS testing" — no .bru files.

### .bru file canonical shape (verified)

```bru
meta {
  name: Login (admin)
  type: http
  seq: 1
}

post {
  url: {{base_url}}/api/auth/login
  body: json
  auth: none
}

headers {
  content-type: application/json
}

body:json {
  {
    "org_id": "{{org_code}}",
    "login": "{{admin_login}}",
    "password": "{{admin_password}}"
  }
}

script:post-response {
  if (res.status === 200) {
    bru.setEnvVar("access_token", res.body.access_token, { persist: true });
    bru.setEnvVar("refresh_token", res.body.refresh_token, { persist: true });
  }
}

tests {
  test("200 OK on valid credentials", function() {
    expect(res.status).to.equal(200);
  });
  test("response contains access + refresh tokens", function() {
    expect(res.body).to.have.property("access_token");
    expect(res.body).to.have.property("refresh_token");
  });
}
```

### Auth pattern across the collection

- Login → `script:post-response` does `bru.setEnvVar("access_token", res.body.access_token, { persist: true })`.
- Authenticated requests use the `auth:bearer { token: {{access_token}} }` block — Bruno injects `Authorization: Bearer <token>` automatically.
- Logout → `script:post-response` clears the tokens (`bru.setEnvVar("access_token", "", { persist: true })`).

### Environments

`environments/smoke.bru`:
```bru
vars {
  base_url: http://127.0.0.1:8080
  org_code: SMOKE-DEFAULT
  admin_login: alice
  admin_password: AlicePass123!
  operator_login: op
  operator_password: OpPass123!
}
```

`environments/dev.bru` — points at `make dev-up` defaults (PG/Redis/NATS via docker-compose.dev.yml, cmd/api via `go run`). Likely `base_url: http://127.0.0.1:8080`, same login defaults.

The `tenant_id` / `user_id` / `project_id` vars get set by post-response scripts of the canonical "create" flows.

### Error-case convention

For every endpoint, ship at least ONE negative request (separate .bru file, suffix `_invalid.bru` or in a `_errors/` subfolder) covering:

- 401 unauthenticated (auth: none on a protected route)
- 403 wrong role (operator JWT hitting admin endpoint — pre-set `access_token` to a known-operator token via env override or a chained operator-login request)
- 404 cross-tenant (different `tenant_id` env var)
- 400 malformed body (missing required field)

The negative cases are the canonical regression net for the RLS + RBAC + middleware chain — they mirror what smoke scenarios assert programmatically.

## 3. Endpoint inventory (verified 2026-05-16 from `internal/<module>/transport/http/routes.go`)

### auth (`/api/auth/*`, `/api/users/*`)

| Method | Path | Role | Notes |
|---|---|---|---|
| POST | `/api/auth/login` | none | body: `{org_id, login, password}` — verified Plan 21b lesson 11 (org_id, NOT org_code) |
| POST | `/api/auth/login/totp` | none | step-2 of login when totp_enabled=true |
| POST | `/api/auth/refresh` | none | body: `{refresh_token}`; mints new access+refresh (rotation) |
| POST | `/api/auth/logout` | none | body: `{refresh_token}`; revokes in Redis |
| GET | `/api/auth/me` | any | returns current claims |
| POST | `/api/auth/me/password` | any | body: `{old, new}` |
| POST | `/api/auth/me/totp/enroll` | any | returns provisioning_uri + recovery codes |
| POST | `/api/auth/me/totp/confirm` | any | body: `{code}` |
| POST | `/api/auth/me/totp/disable` | any | |
| GET | `/api/auth/me/totp/status` | any | returns `{enabled}` |
| POST | `/api/auth/users` | admin | create user (Plan-22 Task 1 verification: admin group is `/api/auth/users`, NOT `/api/users` — `internal/auth/transport/http/routes.go::auth.Group("/users")` where parent is `/api/auth`) |
| GET | `/api/auth/users` | admin | list users |
| GET | `/api/auth/users/:id` | admin + sameTenant | |
| PATCH | `/api/auth/users/:id/roles` | admin + sameTenant | body: `{roles: ["admin"]}` |
| POST | `/api/auth/users/:id/archive` | admin + sameTenant | soft-delete |
| POST | `/api/auth/users/:id/restore` | admin + sameTenant | |
| POST | `/api/auth/users/:id/reset_password` | admin + sameTenant | |

### crm (`/api/projects/*`, `/api/respondents/*`, `/api/imports/*`)

| Method | Path | Role | Notes |
|---|---|---|---|
| GET | `/api/projects` | operator+ | list (paginated) |
| GET | `/api/projects/:id` | operator+ + sameTenant | |
| GET | `/api/projects/:id/progress` | operator+ + sameTenant | |
| GET | `/api/projects/:id/members` | supervisor+ + sameTenant | |
| POST | `/api/projects` | admin | body: `{code, name}` |
| PATCH | `/api/projects/:id` | admin + sameTenant | |
| POST | `/api/projects/:id/pause` | admin + sameTenant | |
| POST | `/api/projects/:id/resume` | admin + sameTenant | |
| POST | `/api/projects/:id/archive` | admin + sameTenant | |
| POST | `/api/projects/:id/assign` | admin + sameTenant | body: `{operators: [uuid…]}` |
| DELETE | `/api/projects/:id/operators/:opID` | admin + sameTenant | |
| POST | `/api/projects/:id/respondents` | admin + sameTenant | body: `{phone, full_name, …}` |
| POST | `/api/projects/:id/respondents/import` | admin + sameTenant | multipart: `?format=csv&filename=phones.csv` + `file=<bytes>` |
| GET | `/api/projects/:id/respondents` | operator+ + sameTenant | search/paginate |
| GET | `/api/respondents/:id` | operator+ + sameTenant | |
| GET | `/api/respondents/:id/with-phone` | admin + sameTenant | decrypted phone |
| DELETE | `/api/respondents/:id` | admin + sameTenant | soft-delete; PurgeWorker hard-deletes after 30 d |
| GET | `/api/imports/:job_id` | admin | terminal states: `"succeeded"` / `"failed"` |

### surveys (`/api/surveys/*`)

Plan-21b lessons 10-11 are authoritative.

| Method | Path | Role | Notes |
|---|---|---|---|
| GET | `/api/surveys` | operator+ | |
| GET | `/api/surveys/:id` | operator+ + sameTenant | |
| GET | `/api/surveys/:id/versions` | operator+ + sameTenant | |
| GET | `/api/surveys/:id/versions/active` | operator+ + sameTenant | |
| POST | `/api/surveys/:id/preview/run` | operator+ | body: schema + answers |
| POST | `/api/surveys` | admin | body: `{name, description?, primary_mode?}` — NO `code` field (Plan-21b lesson 10) |
| PATCH | `/api/surveys/:id` | admin + sameTenant | |
| POST | `/api/surveys/:id/archive` | admin + sameTenant | |
| POST | `/api/surveys/:id/versions` | admin + sameTenant | body: schema; response = full VersionDTO with `major`/`minor` ints |
| POST | `/api/surveys/:id/versions/:version_id/activate` | admin + sameTenant | returns 204 No Content (Plan-21b lesson 11) |
| POST | `/api/surveys/:id/validate` | admin | body: schema; response: validation report |

### dialer (`/api/sessions/*`, `/api/calls/*`, `/api/operator/*`)

Plan-21b lesson 12 is authoritative.

| Method | Path | Role | Notes |
|---|---|---|---|
| POST | `/api/sessions/start` | operator+ | body: `{project_id}` REQUIRED (lesson 12c); transitions FSM offline → ready |
| POST | `/api/sessions/end` | operator+ | offline |
| POST | `/api/sessions/pause` | operator+ | body: `{reason}` REQUIRED min=1 max=64 (lesson 12d); state="pause" (NOT "paused") |
| POST | `/api/sessions/resume` | operator+ | back to ready |
| GET | `/api/sessions/me` | operator+ | returns current snapshot |
| POST | `/api/calls/:id/status` | operator+ | submit call status (success/wrong-person/dnc-hit/…) |
| POST | `/api/calls/:id/hangup` | operator+ + sameTenant | Plan 21 added the RequireSameTenant guard |
| POST | `/api/operator/verify/start` | supervisor+ | |
| POST | `/api/operator/verify/done` | supervisor+ | |
| POST | `/api/operator/:id/force` | admin | force-transition target operator |
| GET | `/api/operator/ws` | operator+ (via `?token=`) | **WebSocket — NOT in Bruno collection**; documented in README only |

### recording (`/api/calls/:id/recording*`, `/api/recordings/*`)

| Method | Path | Role | Notes |
|---|---|---|---|
| GET | `/api/calls/:id/recording` | admin / supervisor | streams plaintext audio; Range header → 416 (Plan 21b Task 5) |
| GET | `/api/recordings/search` | admin / supervisor | cursor-paginated; response field `sha256` = ciphertext sha256 |
| POST | `/api/calls/:id/recording/verify` | admin | re-runs integrity check |

### realtime (`/api/realtime/ws`)

| Method | Path | Role | Notes |
|---|---|---|---|
| GET | `/api/realtime/ws` | any (subprotocol "sociopulse-v1") | **WebSocket — NOT in Bruno collection** |

### billing (`/api/finance/*`, `/api/finance/tariffs`)

| Method | Path | Role | Notes |
|---|---|---|---|
| GET | `/api/finance/dashboard` | admin ∪ supervisor | period=week\|month\|quarter\|year |
| GET | `/api/finance/projects` | admin ∪ supervisor | |
| GET | `/api/finance/breakdown` | admin ∪ supervisor | |
| GET | `/api/finance/byMonth` | admin ∪ supervisor | |
| GET | `/api/finance/tariffs` | admin ∪ supervisor | |
| PATCH | `/api/finance/tariffs` | admin only | body: tariffs map |

OpenAPI source: `docs/api/billing/v1/openapi.yaml` — use for exact request/response shapes.

### reports (`/api/reports/*`, `/api/reports/jobs/*`)

| Method | Path | Role | Notes |
|---|---|---|---|
| GET | `/api/reports` | admin ∪ supervisor | list available kinds |
| POST | `/api/reports/:kind/export` | admin ∪ supervisor | sync OR async (202 + jobID if period>30d OR rows>100k) |
| POST | `/api/reports/custom` | admin ∪ supervisor | custom report builder |
| GET | `/api/reports/jobs/:jobID` | admin ∪ supervisor | async job status |
| GET | `/api/reports/jobs/:jobID/download` | admin ∪ supervisor | 24h presigned-URL S3 redirect |

OpenAPI source: `docs/api/reports/` — use for exact request/response shapes.

## 4. Gotchas (known traps)

### 4.1 Login DTO is `org_id`, not `org_code`

Plan-21b lesson 11 — verified at `internal/auth/transport/http/dto.go:23`. The wire field is `org_id`; the seed helper carries `OrgCode` semantically (it's the same string, just different field naming between DB column and HTTP DTO).

### 4.2 The smoke harness seeds tenants with `org_code = "SMOKE-DEFAULT"`

When using the `smoke` Bruno environment against the smoke testcontainer stack, `org_code` is whatever the smoke test seeded — typically a per-test string like `"SMOKE-A"`. The Bruno environment carries a stable default (`"SMOKE-DEFAULT"`); if the collection is run against a freshly-booted cmd/api (NOT under a smoke test that seeded its own tenant), the operator must seed a tenant first (direct SQL OR a Bruno `pre-request` script that creates one via the platform-level `/api/internal/tenants` endpoint — if one exists).

### 4.3 Bruno does NOT support WebSocket

`/api/operator/ws` and `/api/realtime/ws` are HTTP-Upgrade endpoints. Bruno's request engine is HTTP only. Document these in README + point to `tests/smoke/wsclient.go` as the canonical WS test surface.

### 4.4 Multipart uploads in Bruno

For `POST /api/projects/:id/respondents/import` (multipart CSV upload), Bruno's `body:multipart-form` block accepts `file: @<path>` to reference a file on disk. The collection ships a sample CSV in `docs/api/collections/sociopulse/fixtures/respondents.csv`; the .bru file references it via relative path. Verify Bruno's multipart-form syntax via `context7` before writing the .bru file.

### 4.5 `script:post-response` runs on EVERY response, not just success

Wrap token capture in `if (res.status === 200) { … }` (or similar) — capturing a token from a 401 response would write `undefined`. Plan-22 Task 1's login request must demonstrate the canonical guard.

### 4.6 Bruno persists env vars to disk by default ONLY with `{ persist: true }`

`bru.setEnvVar("access_token", value)` without the persist option is in-memory only — gone the next CLI invocation. For CI use (`bru run --env smoke`), the access_token MUST persist OR be re-fetched in the same run. Convention: login → persist; logout → set to empty (persist).

### 4.7 Error envelope shape

The platform's canonical error response is `{ "error": "<code>", "message": "<text>" }` (mirrors `internal/<module>/transport/http/error_envelope.go` across modules). For `tests` blocks asserting an error response, check `res.body.error` (the code, like `"recording.range_not_satisfiable"` or `"auth.token_invalid"`).

### 4.8 Smoke harness uses ephemeral 127.0.0.1:N ports

When pointing Bruno at the smoke stack, the cmd/api binds an ephemeral port per test invocation — there's no stable URL. The Bruno `smoke` environment is for use against `make dev-up` (docker-compose, stable port 8080), NOT against the smoke testcontainer harness. The smoke harness is for automated scenario tests; Bruno is for human exploration.

### 4.9 Bruno CLI exit code

`bru run --env <env>` exits non-zero if ANY test fails. Suitable for a future CI job (deferred from Plan 22 scope — captured as Phase-1c-or-later follow-up).

### 4.10 Vocabulary in collection metadata

`meta { name: ... }` strings + folder names + `tests("name", ...)` use CONTEXT.md glossary terms strictly. "Operator" / "Respondent" / "Project" / "Survey" / "Recording" / "Tenant" — NOT "User" (overloaded), "Lead", "Campaign" (out-of-domain).

## 5. Open questions (resolve at execution time)

1. **Should the collection include the negative-case .bru files inline (suffix `_invalid.bru`) or in a `_errors/` subfolder?** Recommendation: inline suffix — Bruno's UI ordering uses `seq` from the meta block, so happy+error sit next to each other. Folder-based isolation makes ad-hoc exploration harder.
2. **Should the collection target only the smoke stack or also production-like env?** Recommendation: ship `smoke.bru` + `dev.bru` environments only. Production env is out of scope — Bruno collection is for dev/test, not for prod ops (use observability surfaces for prod).
3. **Are admin endpoints under `/api/users/*` or `/api/auth/admin/*`?** Verify by reading `internal/auth/transport/http/routes.go` — the route table at line ~83-100 has `admin.POST(...)`; the parent group's prefix determines whether it's `/api/users` or `/api/admin/users` or `/api/auth/admin`. Implementer MUST grep for `auth.Group(` / `Mount(` to confirm.
4. **Bruno CLI in CI: in-scope for Plan 22 or follow-up?** Recommendation: follow-up. Plan 22 ships the collection + README; a future plan adds `bru run --env smoke` as a CI job (and decides whether it gates tag-push). Scope creep otherwise.
5. **WS endpoint documentation: in the same README or a separate doc?** Recommendation: a section in the collection README pointing at `tests/smoke/wsclient.go` + `internal/dialer/transport/http/ws.go`. No separate doc.

## 6. Production lessons (post-execution YYYY-MM-DD)

> Filled in at close-out per CLAUDE.md workflow rule #8. Until then, this section is empty by design.

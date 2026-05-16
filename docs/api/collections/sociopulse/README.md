# sociopulse Bruno collection

This collection mirrors the public HTTP surface of `cmd/api` in
[Bruno](https://www.usebruno.com/) — a Postman-style API client whose
collection format is plain-text `.bru` files (JSON-adjacent, diff-able,
git-friendly). It exists for three audiences:

| Audience | Use |
|---|---|
| Developer | Open in the Bruno UI, edit env vars, click through requests during manual exploration. |
| QA | Run `bru run --env smoke` against a freshly-booted `make dev-up` stack for a smoke-level confidence check before a release tag. |
| New agent | The collection enumerates the public surface in a discoverable, executable form — more pragmatic than reading three separate OpenAPI files. |

The collection complements `tests/smoke/` (automated cross-module
regression scenarios). Same target binary (`cmd/api`); different
operator (human vs `go test`). Smoke is the executable regression net;
this collection is the manual exploration surface that closes Phase 2
of `docs/architecture/10-end-to-end-testing-gaps.md`.

Plan 22 shipped the collection in four tasks: Task 1 scaffold + auth,
Task 2 CRM + surveys, Task 3 dialer + recording + WS doc, Task 4
billing + reports + final polish. As of close-out the collection holds
**81 request `.bru` files** across seven modules — every public HTTP
endpoint of `cmd/api` is represented (only the two WebSocket endpoints
are out of scope; see "WebSocket endpoints" below).

## Layout

```
docs/api/collections/sociopulse/
├── bruno.json                 # collection-level config (version, name, type)
├── environments/
│   ├── smoke.bru              # vars for a `make dev-up` stack seeded with SMOKE-DEFAULT / alice
│   └── dev.bru                # placeholder; copy of smoke.bru today
├── fixtures/
│   └── respondents.csv        # canonical 2-row CSV mirroring tests/smoke/respondent_helpers.go
├── auth/                      # 9 user-side endpoints + 7 admin + 2 negative regressions
│   ├── 01_login.bru           # canonical login flow with JWT auto-capture
│   ├── 02_refresh.bru
│   ├── 03_logout.bru
│   ├── me.bru
│   ├── me_password.bru
│   ├── totp_{enroll,confirm,disable,status}.bru
│   ├── admin/{create_user,list_users,get_user,update_roles,archive_user,restore_user,reset_password}.bru
│   └── _errors/{login_wrong_password,refresh_after_logout}.bru
├── crm/                       # 9 project + 6 respondent + 1 import + 2 negative
│   ├── projects/{list,get,create,update,pause,resume,archive,assign,unassign,progress,members}.bru
│   ├── respondents/{create,import,search,get,get_with_phone,delete}.bru
│   ├── imports/get_status.bru
│   └── _errors/{operator_creates_project_403,cross_tenant_project_get}.bru
├── surveys/                   # 6 read + 5 admin + 2 negative
│   ├── {list,get,versions_list,versions_active,preview_run,validate}.bru
│   ├── admin/{create,update,archive,save_version,activate_version}.bru
│   └── _errors/{activate_nonexistent_404,cross_tenant_survey_get}.bru
├── dialer/                    # 5 session + 2 call + 2 verify + 1 force + 2 negative
│   ├── sessions/{start,end,pause,resume,me}.bru
│   ├── calls/{submit_status,hangup}.bru
│   ├── verify/{start,done}.bru
│   ├── operator/force.bru
│   └── _errors/{cross_tenant_hangup_404,pause_without_reason_400}.bru
├── recording/                 # 3 happy + 2 negative
│   ├── {search,get_audio,verify_checksum}.bru
│   └── _errors/{cross_tenant_stream_404,range_request_416}.bru
├── billing/                   # 4 finance + 2 tariffs + 1 negative
│   ├── finance/{dashboard,projects,breakdown,byMonth}.bru
│   ├── tariffs/{get,patch}.bru
│   └── _errors/operator_patches_tariff_403.bru
├── reports/                   # 3 user + 2 jobs + 1 negative
│   ├── {list_kinds,export,custom}.bru
│   ├── jobs/{get,download}.bru
│   └── _errors/cross_tenant_job_404.bru
└── README.md                  # this file
```

## Install

CLI (CI + scripted runs):
```bash
npm install -g @usebruno/cli
bru --version
```

UI (manual exploration):
download the desktop client from https://www.usebruno.com/ and `File →
Open Collection` against this directory.

## First-time setup — seed a tenant + admin

The `smoke` and `dev` environments default to the canonical credentials
`SMOKE-DEFAULT / alice / AlicePass123!`. They map to a row that does
NOT exist in a freshly-booted database — you have to seed it once.

```bash
# 1. Boot the dev stack (Postgres + Redis + NATS + cmd/api on :8080).
make dev-up

# 2. Seed the tenant + admin user. The hash below is argon2id of
#    "AlicePass123!" produced by pkg/passwords.Default().Hash; you can
#    regenerate it with `go run ./internal/auth/cmd/hashpwd AlicePass123!`
#    if you rotate the smoke password.
psql 'postgres://app:devpass@localhost:5432/sociopulse?sslmode=disable' <<'SQL'
WITH t AS (
  INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
  VALUES (gen_random_uuid(), 'SMOKE-DEFAULT', 'Smoke default tenant', 'active',
          'smoke-kek-default', decode('00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff', 'hex'))
  RETURNING id
)
INSERT INTO users (id, tenant_id, login, password_hash, full_name, email,
                   roles, must_change_pwd, totp_enabled)
SELECT gen_random_uuid(), t.id, 'alice',
       '$argon2id$v=19$m=65536,t=3,p=2$REPLACE_WITH_REAL_HASH$REPLACE_WITH_REAL_HASH',
       'Alice Admin', 'alice@smoke.local',
       ARRAY['admin']::text[], false, false
FROM t;
SQL
```

The seed pattern mirrors
[`tests/smoke/seed.go::SeedTenantAndAdmin`](../../../../tests/smoke/seed.go),
which is the authoritative version — copy its SQL if the schema drifts.
The smoke harness uses `pkg/passwords.Default().Hash(ctx, plainPwd)` at
runtime so it never has to ship a static hash; if you want to mirror
that exactly, run a one-shot Go helper instead of pasting a literal.

## Run

```bash
cd docs/api/collections/sociopulse

# Run one request (canonical login → captures access_token + refresh_token
# into environments/smoke.bru on success). Bruno CLI uses a POSITIONAL
# path argument (NOT --filename) since v3.0+; the path can be a single
# .bru OR a directory (use -r for recursive).
bru run auth/01_login.bru --env smoke

# Run the entire auth folder in seq order.
bru run auth --env smoke -r

# Run the whole collection (all 81 request files; the two env files
# under environments/ are config, not requests).
bru run --env smoke -r
```

Each `bru run` invocation persists env-var changes back into the env
file on disk when the request uses `{ persist: true }` — so after a
successful `01_login.bru` your `environments/smoke.bru` will carry the
captured tokens for the subsequent requests to consume.

Exit code: non-zero if any `tests` block fails. Suitable for a future
CI job (deferred — see "CI integration" below).

## Walk-throughs — end-to-end chained flows

The collection's env-var capture chain lets you drive realistic
journeys through the platform without manual paste. Each walk-through
assumes a freshly-booted `make dev-up` stack with the SMOKE-DEFAULT
tenant + alice admin seeded (see "First-time setup" above).

### Walk-through A — admin onboards a project + respondents

End-to-end coverage: admin auth + project lifecycle + respondent
import. Demonstrates the canonical post-response env-var capture chain
across modules.

```bash
cd docs/api/collections/sociopulse

# 1. Admin login → access_token + tenant_id + user_id captured to env.
bru run auth/01_login.bru --env smoke

# 2. Create a project → project_id captured.
bru run crm/projects/create.bru --env smoke

# 3. Bulk-import respondents from the bundled CSV fixture.
#    Multipart syntax: body:multipart-form { file: @fixtures/respondents.csv }
#    (path is relative to the .bru file).
bru run crm/respondents/import.bru --env smoke

# 4. Poll the import job until it terminates (succeeded | failed).
bru run crm/imports/get_status.bru --env smoke

# 5. Search the imported respondents (filtered by the project).
bru run crm/respondents/search.bru --env smoke

# 6. Read one respondent + its decrypted phone (admin-only).
bru run crm/respondents/get_with_phone.bru --env smoke
```

Env vars used: `access_token`, `tenant_id`, `user_id`, `project_id`,
`respondent_id`. All set automatically by post-response scripts.

### Walk-through B — operator runs a call + supervisor verifies recording

End-to-end coverage: operator session lifecycle + call status
submission + recording chain-of-custody.

```bash
# 1. Operator login → access_token captured.
#    (Swap the env file's admin_login / admin_password for the operator
#    credentials first, OR maintain a separate environments/smoke-op.bru
#    for the operator persona.)
bru run auth/01_login.bru --env smoke

# 2. Bind the operator to a project (state offline → ready).
bru run dialer/sessions/start.bru --env smoke

# 3. Read the current FSM snapshot (state machine + bound project).
bru run dialer/sessions/me.bru --env smoke

# 4. Wait for the auto-dispatcher to pick a respondent + open a call.
#    Read current_call_id off the operator's WS snapshot (see
#    "WebSocket endpoints" below) — the dialer module has no direct
#    "create a call" REST endpoint; calls are minted by the dispatch
#    loop in internal/dialer/service/router.go::Dial.
#    Set {{call_id}} + {{respondent_id}} env vars manually.

# 5. Submit the call status disposition (success / refused / wrong_person
#    / dropped / no_answer / busy / callback / tech_failure).
bru run dialer/calls/submit_status.bru --env smoke

# 6. Swap to admin / supervisor JWT (re-login as alice) for recording access.
bru run auth/01_login.bru --env smoke

# 7. Search the recording for that call → recording_call_id captured.
bru run recording/search.bru --env smoke

# 8. Stream the decrypted audio (200 + audio/ogg bytes).
bru run recording/get_audio.bru --env smoke

# 9. Verify the SHA-256 chain-of-custody integrity.
bru run recording/verify_checksum.bru --env smoke
```

### Walk-through C — admin reviews finance + runs an async report

End-to-end coverage: billing dashboard + tariff editor + reports
sync / async routing.

```bash
# 1. Admin login.
bru run auth/01_login.bru --env smoke

# 2. Pull the dashboard composite payload (KPI + breakdown +
#    byMonth + top-projects).
bru run billing/finance/dashboard.bru --env smoke

# 3. Read tariff snapshot → tariff_version captured.
bru run billing/tariffs/get.bru --env smoke

# 4. PATCH the tariff. Post-response asserts version strictly
#    greater than the pre-PATCH value captured in step 3.
bru run billing/tariffs/patch.bru --env smoke

# 5. Re-fetch tariffs; the response carries the bumped version.
bru run billing/tariffs/get.bru --env smoke

# 6. Submit an export request. If window > 30 days, the handler
#    auto-routes to async and returns 202 + report_job_id (captured).
#    Otherwise it returns 200 + raw bytes.
bru run reports/export.bru --env smoke

# 7. Poll job status until State="succeeded".
bru run reports/jobs/get.bru --env smoke

# 8. Download the rendered artifact (302 → 24h presigned URL).
bru run reports/jobs/download.bru --env smoke
```

Env vars used: `access_token`, `tariff_version`, `report_job_id`.

## Extend — write a new `.bru`

The canonical reference is
[`auth/01_login.bru`](auth/01_login.bru). Every new request follows
the same shape:

```bru
meta {
  name: Human-readable name (uses CONTEXT.md glossary terms)
  type: http
  seq: 1
}

post {
  url: {{base_url}}/api/<module>/<endpoint>
  body: json
  auth: bearer  # or `none` for unauthenticated routes
}

auth:bearer {
  token: {{access_token}}
}

headers {
  content-type: application/json
}

body:json {
  { "field": "{{env_var}}" }
}

script:post-response {
  // Guard on status so a 4xx does not overwrite captured tokens with
  // undefined. `persist: true` writes the value back to the env file.
  if (res.status === 200 && res.body && res.body.id) {
    bru.setEnvVar("entity_id", res.body.id, { persist: true });
  }
}

tests {
  test("200 OK on happy path", function() {
    expect(res.status).to.equal(200);
  });
}
```

### Conventions

- **Vocabulary** — `meta.name` strings, folder names, and `tests(...)`
  descriptions use [`CONTEXT.md`](../../../../CONTEXT.md) glossary
  terms only (Operator, Respondent, Project, Survey, Recording,
  Tenant). Avoid overloaded words like "User" outside the auth module.
- **Negative cases** — every endpoint with a security boundary gets a
  paired `.bru` under `_errors/` covering one of: 401 (unauthenticated),
  403 (wrong role), 404 (cross-tenant), 400 (malformed body). Use
  `seq: 10+` for negatives so they sit AFTER happy paths in the
  Bruno UI ordering. The CLI walks subfolders alphabetically — that
  means `_errors/` (underscore sorts before letters) runs FIRST in a
  recursive sweep, so negative cases must stand alone (no dependency
  on the happy-path having run first in the same `bru run` invocation).
- **JWT capture** — only the canonical login flow persists tokens.
  Every other authenticated request reads `{{access_token}}` via
  `auth:bearer`. The logout flow clears the captured tokens on 204 so
  the env file does not carry a stale, revoked refresh.
- **Variable persistence** — `bru.setEnvVar(key, value)` is in-memory
  only; add `{ persist: true }` to write back to the env file on disk.
  CLI runs operate on the persisted file, so chained requests across
  separate invocations require persistence.

## Common gotchas — distilled from Plan 22 implementation

These are the production lessons that bit us during Plan 22's four
tasks. Future agents extending the collection should read these first.

1. **Top-level `#` comments fail the Bruno parser.** Bruno's grammar
   only allows `#` inside specific blocks; a free-standing comment at
   the top of a `.bru` file aborts parsing with a syntax error. Use
   the `docs { ... }` block (parsed as multi-line free-text and shown
   in the Bruno UI's "Docs" tab) instead of leading comments.

2. **Bruno CLI walks subfolders alphabetically.** When you run
   `bru run <module> -r --env smoke`, `_errors/` (underscore sorts
   first) runs BEFORE the happy-path siblings. Negative cases must
   therefore be self-contained — never depend on a happy-path
   creating state the negative case will read. Use a separate
   pre-existing env var (seeded out-of-band) for any state the
   negative case needs.

3. **CLI uses POSITIONAL path arg, not `--filename`.** The pre-3.0
   docs example `bru run --filename foo.bru` is stale. Modern syntax
   is `bru run foo.bru --env smoke` (path first, flags after). The
   path can be a single file OR a directory; `-r` makes it recursive.

4. **`script:post-response` runs on EVERY response, not just success.**
   Wrap env-var captures in `if (res.status === 200 && res.body && ...)`
   so a 401 / 4xx does not overwrite a previously-captured token /
   id with `undefined`. The canonical login flow at
   `auth/01_login.bru` demonstrates the guard pattern.

5. **`bru.setEnvVar` is in-memory only by default.** Add
   `{ persist: true }` for the value to write back to the env file on
   disk. CLI runs share the env file across invocations only when the
   capturing request persisted; otherwise the value is gone on the
   next `bru run`. Convention: login → persist, logout → set to empty
   + persist (so the env file does not carry a revoked token).

6. **Wire-format reality is non-uniform across modules.** Several
   common-looking endpoints diverge from their plan-text descriptions:

   - **`DELETE /api/respondents/:id`** returns **200 + DeletionReceiptDTO**,
     NOT 204 (the handler surfaces the scheduled purge timestamp to
     the caller).
   - **`POST /api/calls/:id/hangup`** returns **204 No Content** (the
     FSM transition is asynchronous via NATS; the HTTP response
     carries no body).
   - **`POST /api/calls/:id/status`** body enum is exactly 8
     underscore-separated values: `success`, `refused`, `wrong_person`,
     `dropped`, `no_answer`, `busy`, `callback`, `tech_failure`.
   - **`POST /api/surveys/:id/versions/:vid/activate`** returns **204
     No Content** (NOT a SaveVersionResponse).
   - **`POST /api/sessions/pause`** body field is `state: "pause"`
     (NOT `"paused"`) and the `reason` field is REQUIRED with min=1
     max=64 chars.
   - **`PATCH /api/billing/tariffs`** body has NO `expected_version`
     field — last-writer-wins, not optimistic concurrency. The version
     is bumped server-side.
   - **`POST /api/reports/:kind/export`** body has NO `period` field —
     uses RFC 3339 `window_from` + `window_to` instead.
   - **`POST /api/reports/custom`** is ALWAYS 202 (never 200). Predefined
     export tries sync first; if window > 30 days OR estimated rows >
     100k, the Runner returns `ErrAsyncRequired` and the handler
     auto-routes to 202 + JobTicket.
   - **`GET /api/reports/jobs/:jobID/download`** returns **302 redirect**
     to the 24h presigned URL (NOT 200 + JSON envelope).

   Read every `dto.go` / `handlers.go` / `*_test.go` field name + status
   code verbatim. The `.bru` files in this collection are the
   authoritative wire contract because they assert against the real
   handlers; if a future doc disagrees, the `.bru` wins.

7. **Error envelope shape differs across modules.** This is the
   highest-leverage trap:

   | Module | Envelope field | Code example |
   |---|---|---|
   | `auth/*` | `error` | `auth.insufficient_role`, `auth.token_invalid` |
   | `crm/*` | `error` | `auth.insufficient_role` (RBAC reuses auth) |
   | `surveys/*` | `error` | (auth envelope on RBAC, surveys-specific on validation) |
   | `dialer/*` | `code` | `dialer.bad_request`, `dialer.invalid_state` |
   | `recording/*` | `code` | `recording.range_not_satisfiable`, `recording.not_found` |
   | `billing/*` | `code` | `billing.forbidden`, `billing.invalid_tariff`, `billing.no_tariffs` |
   | `reports/*` | `code` | `reports.job_not_found`, `reports.unknown_kind`, `reports.too_large` |

   When writing a negative `.bru`, verify which envelope field the
   target module uses BEFORE writing the test assertion. The
   `internal/<module>/transport/http/error_envelope.go` (or
   `dto.go::ErrorEnvelope`) is the authoritative source. Five of seven
   modules use `code`; only `auth` (and the CRM/surveys RBAC chain
   that wraps auth's middleware) uses `error`.

8. **Multipart syntax in Bruno.** For `POST /api/projects/:id/respondents/import`
   the body block is:

   ```bru
   body:multipart-form {
     file: @fixtures/respondents.csv
   }
   ```

   The `@<path>` syntax loads the file from disk at request-time; the
   path is relative to the `.bru` file's directory. The collection
   ships a canonical CSV at `fixtures/respondents.csv` mirroring
   `tests/smoke/respondent_helpers.go`'s seed format.

9. **Query parameters require the `params:query` block** (in addition
   to inlining them in the URL). Bruno parses both, but the
   `params:query` block is the one rendered in the UI's Params tab;
   omitting it makes the UI think the URL is hard-coded. Convention
   for the collection: inline the param in the URL `url:` line AND
   list it in `params:query`, so the UI and CLI agree.

10. **`tests` block uses Chai-flavoured assertions.** `expect().to.equal()`
    for primitives, `expect().to.have.property()` for object keys,
    `expect().to.be.an("array")` for type checks, `expect([a,b]).to.include(x)`
    for set membership. Bruno docs: https://docs.usebruno.com/testing.

## WebSocket endpoints — NOT covered by Bruno

Bruno is HTTP-only — its request engine speaks HTTP and the upgrade
handshake that promotes a request to a WebSocket frame stream is
outside that surface. The platform ships two WS endpoints; both are
covered by the Go smoke harness instead of Bruno.

| Endpoint | Module | Auth path | Frame contract |
|---|---|---|---|
| `GET /api/operator/ws` | dialer (operator-facing real-time) | `?token=<jwt>` query parameter | Server-pushed bare `SnapshotDTO` JSON frames (no `{type:...}` envelope; per Plan-21b lesson 12a) |
| `GET /api/realtime/ws` | realtime (generic per-replica Hub) | FrameAuth handshake (token sent as the first frame, NOT as a query parameter) | `internal/realtime/api/events.go` event envelope |

Both endpoints are mounted OUTSIDE the gin `JWTMiddleware` chain
(verified at
[`internal/dialer/transport/http/routes.go:141-147`](../../../../internal/dialer/transport/http/routes.go)
and
[`internal/realtime/transport/http/routes.go:97-108`](../../../../internal/realtime/transport/http/routes.go))
because browsers cannot easily set the `Authorization` header on a
WebSocket handshake. The dialer's handler reads the token from
`c.Query("token")` (with an `Authorization: Bearer <jwt>` header
fallback for non-browser clients), self-validates against
`Deps.Validator`, and enforces the operator-role gate in-line; the
realtime handler validates the token from the first wire frame
(FrameAuth handshake on
[`internal/realtime/transport/http/ws_handler.go`](../../../../internal/realtime/transport/http/ws_handler.go)).

### Canonical Go-side WS test surface

The smoke harness exposes a deliberately tiny wrapper at
[`tests/smoke/wsclient.go`](../../../../tests/smoke/wsclient.go) — the
authoritative reference for any new WS scenario:

```go
//go:build smoke

ws, err := smoke.DialOperator(t.Context(), t, addr, jwt)
require.NoError(t, err)
t.Cleanup(func() { _ = ws.Close() })

// Each ReadEvent reads exactly one JSON frame with a per-call timeout.
snap, err := ws.ReadEvent(t.Context(), 5*time.Second)
require.NoError(t, err)
require.Equal(t, "ready", snap["state"])
```

The wrapper owns the `*websocket.Conn` from
[`github.com/coder/websocket`](https://github.com/coder/websocket),
decodes each frame into `map[string]any` (typed shape would force a
schema PR every time the dialer's wire format grows a field), and
issues a normal-closure frame on `Close` so the server-side handler's
deferred subscription release runs cleanly. `goleak.VerifyTestMain`
in the smoke package keeps any leak honest.

For ad-hoc WS exploration outside the smoke harness, point a generic
WS client (e.g. [`websocat`](https://github.com/vi/websocat)) at
`ws://127.0.0.1:8080/api/operator/ws?token=<jwt>` after running the
Bruno login flow to obtain a fresh access token:

```bash
# 1. Capture a fresh JWT via Bruno (writes access_token to env on disk).
bru run auth/01_login.bru --env smoke

# 2. Read it back + open a WS stream with websocat.
ACCESS_TOKEN=$(grep '^  access_token:' environments/smoke.bru | awk '{print $2}')
websocat "ws://127.0.0.1:8080/api/operator/ws?token=$ACCESS_TOKEN"
```

The realtime `?token=` query path is documented above for the dialer
endpoint specifically — the realtime WS handler requires the token
inside the first FrameAuth frame, so `websocat` against it needs a
prepared scripted payload. The smoke harness is the path of least
resistance for realtime scenarios.

## References

- Plan 22 spec:
  [`docs/superpowers/plans/2026-05-16-22-rest-collection.md`](../../../superpowers/plans/2026-05-16-22-rest-collection.md)
- Plan 22 references (per-module endpoint inventory + gotchas):
  [`docs/references/plan-22-rest-collection.md`](../../../references/plan-22-rest-collection.md)
- Wire-format reality (Plan 21b lessons 10–15):
  [`docs/references/plan-21b-phase-1b-smoke-scenarios.md`](../../../references/plan-21b-phase-1b-smoke-scenarios.md)
- Per-module OpenAPI specs (where they exist):
  [`docs/api/billing/`](../billing/) (canonical billing API),
  [`docs/api/recording/`](../recording/) (recording API),
  [`docs/api/reports/`](../reports/) (reports API).
  Other modules (auth, crm, surveys, dialer, realtime) have no
  standalone OpenAPI doc; the `internal/<module>/transport/http/dto.go`
  files are the wire contract source.
- Automated smoke regression net + WS coverage:
  [`tests/smoke/`](../../../../tests/smoke/) — 10 smoke scenarios
  exercising the same endpoints this collection enumerates, plus the
  two WS surfaces that fall outside Bruno's scope.
- Bruno docs: https://docs.usebruno.com/

## Future work — CI integration (deferred)

`bru run --env smoke` as a CI job is intentionally out of scope for
Plan 22. The collection ships in this plan; a future plan decides
whether to:

1. Add a `rest` job to the GitHub Actions matrix.
2. Stand up the dependencies the job needs (`make dev-up` in CI is
   non-trivial — Postgres + Redis + NATS + cmd/api all need to be
   spun up; the smoke harness's testcontainers seam is the natural
   reuse target).
3. Decide whether the job gates tag-push or is informational-only.

The smoke test layer (`go test -tags=smoke ./...`) is the executable
regression net today and remains the gating signal until the CI
integration plan lands.

Phase 2 of [`docs/architecture/10-end-to-end-testing-gaps.md`](../../../architecture/10-end-to-end-testing-gaps.md)
is **closed** by this collection — every public HTTP endpoint of
`cmd/api` is now exercisable from a developer-friendly UI tool, and
the new-agent onboarding scenario the Phase 2 closure plan targeted
is unblocked.

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
operator (human vs `go test`).

Plan 22 ships the collection in four tasks; this README describes the
scaffold + auth module landed in Task 1. CRM, surveys, dialer,
recording, billing, and reports land in Tasks 2–4.

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
│   ├── totp_enroll.bru
│   ├── totp_confirm.bru
│   ├── totp_disable.bru
│   ├── totp_status.bru
│   ├── admin/
│   │   ├── create_user.bru
│   │   ├── list_users.bru
│   │   ├── get_user.bru
│   │   ├── update_roles.bru
│   │   ├── archive_user.bru
│   │   ├── restore_user.bru
│   │   └── reset_password.bru
│   └── _errors/
│       ├── login_wrong_password.bru
│       └── refresh_after_logout.bru
└── README.md                  # this file
```

Tasks 2–4 add `crm/`, `surveys/`, `dialer/`, `recording/`, `billing/`,
`reports/` siblings under `sociopulse/`. Each follows the same
convention: HTTP-verb folder layout, `seq` ordering for happy-path,
`_errors/` subfolder for negative cases.

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
# into environments/smoke.bru on success).
bru run --env smoke --filename auth/01_login.bru

# Run the entire auth folder in seq order.
bru run --env smoke --filename auth

# Run the whole collection (Tasks 2–4 will populate the rest).
bru run --env smoke
```

Each `bru run` invocation persists env-var changes back into the env
file on disk when the request uses `{ persist: true }` — so after a
successful `01_login.bru` your `environments/smoke.bru` will carry the
captured tokens for the subsequent requests to consume.

Exit code: non-zero if any `tests` block fails. Suitable for a future
CI job (deferred — see "CI integration" below).

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
  403 (wrong role), 404 (cross-tenant), 400 (malformed body). Keep them
  in the same folder as their sibling so a UI walk-through reads them
  next to the happy path; ordering is via `seq` (negative cases
  conventionally `seq: 10+`).
- **JWT capture** — only the canonical login flow persists tokens.
  Every other authenticated request reads `{{access_token}}` via
  `auth:bearer`. The logout flow clears the captured tokens on 204 so
  the env file does not carry a stale, revoked refresh.
- **Variable persistence** — `bru.setEnvVar(key, value)` is in-memory
  only; add `{ persist: true }` to write back to the env file on disk.
  CLI runs operate on the persisted file, so chained requests across
  separate invocations require persistence.

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
bru run --env smoke --filename auth/01_login.bru

# 2. Read it back + open a WS stream with websocat.
ACCESS_TOKEN=$(grep '^  access_token:' environments/smoke.bru | awk '{print $2}')
websocat "ws://127.0.0.1:8080/api/operator/ws?token=$ACCESS_TOKEN"
```

The realtime `?token=` query path is documented above for the dialer
endpoint specifically — the realtime WS handler requires the token
inside the first FrameAuth frame, so `websocat` against it needs a
prepared scripted payload. The smoke harness is the path of least
resistance for realtime scenarios.

## CI integration — deferred

`bru run --env smoke` as a CI job is intentionally out of scope for
Plan 22. The collection ships in this plan; a future plan decides
whether to add a `rest` job to the GitHub Actions matrix and whether it
gates tag-push. The smoke test layer (`go test -tags=smoke ./...`) is
the executable regression net today.

## References

- Plan 22 spec:
  [`docs/superpowers/plans/2026-05-16-22-rest-collection.md`](../../../superpowers/plans/2026-05-16-22-rest-collection.md)
- Plan 22 references:
  [`docs/references/plan-22-rest-collection.md`](../../../references/plan-22-rest-collection.md)
- Wire-format reality (Plan 21b lessons 10–15):
  [`docs/references/plan-21b-phase-1b-smoke-scenarios.md`](../../../references/plan-21b-phase-1b-smoke-scenarios.md)
- Per-module OpenAPI specs (where they exist):
  [`docs/api/billing/`](../billing/),
  [`docs/api/recording/`](../recording/),
  [`docs/api/reports/`](../reports/).
- Bruno docs: https://docs.usebruno.com/

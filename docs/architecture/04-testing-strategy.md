# 04. Testing Strategy

This is the strategic counterpart to `08-tdd-discipline.md`. The TDD
document tells you how the Red-Green-Refactor loop runs inside one task;
this document tells you which kinds of tests we have, what each kind is
for, and what coverage we expect. Both documents together describe the
project's testing surface.

The pyramid we build to (spec §17.1):

```
                ┌───────────────────────┐
                │  Manual / UAT  ~20    │   per release; documented in QA wiki
                └───────────────────────┘
              ┌──────────────────────────┐
              │  E2E (Playwright)  ~50   │   PR-time fast subset; main = full
              └──────────────────────────┘
            ┌────────────────────────────┐
            │  Integration   ~200        │   testcontainers, //go:build integration
            └────────────────────────────┘
          ┌──────────────────────────────┐
          │  Unit       ~2 000           │   every commit
          └──────────────────────────────┘
```

## Layer 1 — Unit

Unit tests are the project's bread and butter. They:

- Run in milliseconds (target <1 ms each, hard ceiling 10 ms).
- Use no out-of-process dependencies. Postgres / Redis / NATS / S3 /
  KMS / FreeSWITCH are stubbed via the `api/` interfaces of their owning
  module.
- Live next to the code they exercise:
  `internal/<module>/service/<file>_test.go`.
- Cover every code path in `service/`, including the unhappy paths
  that drive the sentinel errors of `02-module-contracts.md`.

Mocking is via **mockery v2** (per ADR-0016 candidate; locked in Plan
00a Task 7) generating one mock file per `api/` interface:

```
internal/<module>/api/mocks/
├── authenticator_mock.go
├── user_service_mock.go
├── ...
```

Generation is driven by `.mockery.yaml` at the repo root and a
`go:generate` directive on each `api/` interface. Hand-rolled mocks are
forbidden — they drift from the interface and complicate refactors.
`testify/mock` and `gomock` were both evaluated; `mockery` (which uses
`testify/mock` under the hood but generates the mock from the
interface) is the chosen tool.

A representative shape, drawn from
`samber/cc-skills-golang@golang-testing`:

```go
// internal/auth/service/authenticator_test.go
package service_test

import (
    "context"
    "errors"
    "testing"

    "github.com/google/uuid"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    authapi "github.com/sociopulse/platform/internal/auth/api"
    "github.com/sociopulse/platform/internal/auth/api/mocks"
    "github.com/sociopulse/platform/internal/auth/service"
)

func TestAuthenticator_Login_BadPassword_ReturnsErrInvalidCredentials(t *testing.T) {
    t.Parallel()

    tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
    userID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

    users := mocks.NewUserStore(t)
    hasher := mocks.NewPasswordHasher(t)
    issuer := mocks.NewJWTIssuer(t)

    users.EXPECT().GetByLogin(t.Context(), tenantID, "alice").
        Return(authapi.User{ID: userID, TenantID: tenantID, Login: "alice"}, "argon2id$...", nil)
    hasher.EXPECT().Verify("wrong", "argon2id$...").Return(authapi.ErrInvalidCredentials)

    a := service.NewAuthenticator(service.AuthDeps{
        Users:  users, Hasher: hasher, Issuer: issuer,
        // ... rate-limiter, lockout, totp, audit
    })

    _, err := a.Login(t.Context(), authapi.LoginInput{
        OrgID: "CC-MOSKVA-01", Login: "alice", Password: "wrong",
    })
    assert.True(t, errors.Is(err, authapi.ErrInvalidCredentials))
}
```

Key invariants:

- **`t.Parallel()`** at the top of every test (and every subtest in a
  table). `paralleltest` enforces this.
- **`t.Context()`** (Go 1.24+) instead of `context.Background()` —
  cancels on test failure / cleanup.
- **`require.X` for setup, `assert.X` for properties.** A failed
  preflight short-circuits the rest of the test (`require`); once we're
  past setup, individual property checks accumulate (`assert`).
- **Named subtests** for table-driven cases:
  `t.Run("zero quantity returns zero", func(t *testing.T) { ... })`. So
  `go test -run TestX/<name>` selects one case.
- **No `testify/suite`.** It hides the test boundary, breaks
  `t.Parallel()` at subtest level, and conflicts with `goleak`. Stated
  in `08-tdd-discipline.md`.

## Layer 2 — Integration

Integration tests exercise real adapters against ephemeral
infrastructure. They live next to unit tests, gated by build tag:

```go
//go:build integration

package store_test

import (
    "context"
    "testing"

    "github.com/jackc/pgx/v5"
    "github.com/stretchr/testify/require"
    "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestUserStore_CreateUser_PersistsRow(t *testing.T) {
    t.Parallel()

    ctx := t.Context()
    pgC, err := postgres.Run(ctx, "postgres:16",
        postgres.WithDatabase("sociopulse_test"),
        postgres.WithUsername("test"), postgres.WithPassword("test"),
    )
    require.NoError(t, err)
    t.Cleanup(func() { _ = pgC.Terminate(ctx) })

    dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
    require.NoError(t, err)

    pool := mustMigrateAndConnect(t, ctx, dsn)
    // ... actual test
}
```

Run integration tests separately:

```bash
go test -tags=integration -race -count=1 ./...
```

CI runs the unit suite on every PR and the integration suite on PRs
that touch `internal/.../store/`, `migrations/`, or `pkg/postgres`. The
main-branch CI runs both unconditionally.

What deserves an integration test:

- Every `store/` package — confirms SQL syntax, RLS policies, index
  usage. Fixture data is loaded via `testdata/<table>.sql` files.
- Every `events/` subscriber — uses a real `nats-server/v2` running
  in-process. Confirms durable consumer config, ack policy, retry
  semantics.
- Every gRPC server — uses `bufconn` for in-memory transport.
  Confirms interceptor chain, mTLS via fixture certs, idempotency.
- Cross-module flows hitting real Postgres + Redis + NATS:
  - `dialer.PickNext` against real ZSET + Lua scripts.
  - `recording.Commit` end-to-end with MinIO + KMS fake.
  - `surveys.SurveyService.SaveVersion` against real Postgres
    (validates the JSON-Schema migration is in sync).
- Telephony bridge — runs `signalwire/freeswitch:1.10.10` in Docker,
  exercises Originate → CHANNEL_ANSWER → Hangup.

Containers run **per test**, not shared. The cost is ~250 ms startup
per Postgres container; we accept this for full isolation. When that
cost becomes the bottleneck, we will revisit shared-container
strategies in a separate ADR — not before.

## Layer 3 — End-to-End

Frontend E2E uses Playwright (`tests/e2e/`), running TypeScript against
an ephemeral Kubernetes cluster (`kind`) brought up by CI. Scenarios
mirror the eight admin-flow walkthroughs in
`docs/api/e2e-scenarios.md`:

1. Operator: login → workstation → ready → call → answer → save.
2. Admin: project create → import respondents → assign operator.
3. Admin: survey create in form-mode → switch to flow-mode → preview.
4. Supervisor: listen-in silent → mark violation → audit visible.
5. Admin: tariff edit → next call billed correctly.
6. Operator: 2FA enroll → next login requires TOTP.
7. Admin: force-end-shift → operator sees disconnect frame.
8. 152-ФЗ subject right: delete respondent → 30 d later worker purges.

PR runs the fast subset (1, 3, 4, 5). Main runs all eight. Tests
record screenshots on failure to GitHub Actions artifacts (90-day
retention).

## Coverage Targets

Per layer (`08-tdd-discipline.md` repeats this table):

| Layer | Target |
|---|---|
| `internal/<module>/service/` | ≥ 85% |
| `internal/<module>/store/` | ≥ 70% |
| `internal/<module>/http/` (gin handlers) | ≥ 60% |
| `internal/<module>/grpc/` | ≥ 60% |
| `pkg/*` | ≥ 80% |
| `cmd/<binary>/main.go` | smoke — `/healthz` returns 200 |

**Critical paths** ≥ 90%, every branch:

- `internal/dialer/service` — OperatorFSM, CallQueue, RDDGenerator.
- `internal/surveys/service` — Runtime (next-node, validate, progress).
- `internal/telephony/service` — Router (least-cost-with-fallback).
- `internal/recording/service` — integrity verification, retention
  scheduler.
- `pkg/encryption` — AES-256-GCM wrap/unwrap.
- `pkg/passwords` — argon2id PHC encode/decode.

The CI gate: `make test-cover` runs `go test -race -count=1 -cover ./...`,
then a small `tools/coverage-gate` binary parses the output and fails
the build if any of the above falls below its target. The gate is
strict on critical paths and warning-only on tier-2 paths during the
first quarter of the project — the threshold escalates after we have a
green main for two weeks running.

## Race Detector

Every CI run uses:

```bash
go test -race -count=1 ./...
```

`-count=1` defeats the test cache so the race detector actually runs.
Local development can omit `-count=1` for speed; CI must include it.

A race detector finding is **never** a flaky test. It is a real bug.
Fix the race; never `// nolint:race` it. The
`samber/cc-skills-golang@golang-troubleshooting` skill has the
diagnostic playbook (`go run -race`, `GOMAXPROCS=2 go test -race`,
detector-friendly logging).

## Goroutine Leak Detection

Every package that spawns a goroutine in production code installs
`go.uber.org/goleak.VerifyTestMain` in a `main_test.go`:

```go
package service

import (
    "testing"

    "go.uber.org/goleak"
)

func TestMain(m *testing.M) {
    goleak.VerifyTestMain(m)
}
```

Mandatory in:

- `pkg/outbox/`
- `internal/dialer/service/`
- `internal/realtime/service/`
- `internal/telephony/`
- `internal/telephony/pool/`
- `internal/telephony/nats_bridge/`
- `internal/recording/service/`
- `internal/analytics/service/` (per-subject consumer goroutines)
- `internal/reports/service/` (asynq job runner)

If a known third-party library leaks (e.g. pgx pool background health
checker), exclude it explicitly:

```go
goleak.VerifyTestMain(m,
    goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
)
```

Never use `goleak.IgnoreCurrent()` — it ignores ALL goroutines alive
at test start, masking real leaks introduced by the test under
development.

## Concurrency Patterns We Test For

Every concurrent code path must have a test that runs under `-race`
and exercises:

- **Cancellation propagation.** Pass `t.Context()`, cancel it, verify
  goroutines exit within 100 ms.
- **Context-aware blocking primitives.** Channel reads pair with
  `ctx.Done()`; tests assert that a cancelled context releases
  blocked readers.
- **`errgroup.WithContext` + `SetLimit`.** A worker pool of N must
  not start the (N+1)-th goroutine until one finishes. Tests use a
  fake clock + counter to assert this.
- **Channel ownership.** Only senders close. A test sending on a
  closed channel reproduces the bug.
- **No `time.After` in loops** — use `time.NewTimer + Reset`. Reviewer
  checks; CI grep guard `make grep-time-after` (Plan 00a Task 8).

## TDD Discipline

TDD is **mandatory** per ADR-0015. Every task in every plan is one
Red-Green-Refactor cycle. The full playbook lives in
`08-tdd-discipline.md`; the headline:

1. **Red.** Write the failing test that captures the new behaviour.
   Confirm it fails for the right reason — absence of the production
   code, not a typo.
2. **Green.** Write the minimum production code to make the test pass.
3. **Refactor.** With the test as a safety net, improve names, extract
   helpers, deduplicate. Run tests after every refactor step.

Allowed exceptions (see ADR-0015):

- `cmd/<binary>/main.go` — composition root, smoke-test only.
- Migrations — schema validated through integration tests.
- Generated code (mocks, proto stubs).

A `// characterization: pre-existing behaviour` comment marks tests
written *after* the code (legacy capture). Reviewers flag these for
extra scrutiny.

## Gin-Specific HTTP Test Pattern

For a gin handler:

```go
// internal/auth/http/login_test.go
package http_test

import (
    "bytes"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/gin-gonic/gin"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    authapi "github.com/sociopulse/platform/internal/auth/api"
    httpsrv "github.com/sociopulse/platform/internal/auth/http"
)

func TestLoginHandler_BadCredentials_Returns401(t *testing.T) {
    t.Parallel()

    gin.SetMode(gin.TestMode)

    svc := &fakeAuthService{loginErr: authapi.ErrInvalidCredentials}
    h := httpsrv.NewHandler(svc)

    body, err := json.Marshal(authapi.LoginInput{
        OrgID: "CC-MOSKVA-01", Login: "alice", Password: "wrong",
    })
    require.NoError(t, err)

    req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    rec := httptest.NewRecorder()

    r := gin.New()
    r.POST("/api/auth/login", h.Login)
    r.ServeHTTP(rec, req)

    assert.Equal(t, http.StatusUnauthorized, rec.Code)

    var errResp struct {
        Error struct{ Code, Message string } `json:"error"`
    }
    require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
    assert.Equal(t, "auth.invalid_credentials", errResp.Error.Code)
}
```

`gin.SetMode(gin.TestMode)` is the **first** line of every test file's
`TestMain` (or the first line of every test function if no `TestMain`).
It silences gin's default logger so the test output stays readable.

For unit-testing a handler in isolation (without routing), use
`gin.CreateTestContext`:

```go
rec := httptest.NewRecorder()
c, _ := gin.CreateTestContext(rec)
c.Request = httptest.NewRequest(http.MethodGet, "/x?foo=bar", nil)
c.Params = gin.Params{{Key: "id", Value: "42"}}

h.GetUser(c)

assert.Equal(t, http.StatusOK, rec.Code)
```

The full pattern is documented in
`08-tdd-discipline.md` § "Gin-specific HTTP Test Pattern".

## Fixtures and Golden Files

For complex outputs (rendered XML dialplan, JSON survey schema, gin
error envelopes), use `testdata/` golden files:

```go
got, err := dialplan.Render(ctx, project)
require.NoError(t, err)

golden := filepath.Join("testdata", "dialplan_simple.xml")
if *update {
    require.NoError(t, os.WriteFile(golden, got, 0o644))
}

want, err := os.ReadFile(golden)
require.NoError(t, err)
assert.Equal(t, string(want), string(got))
```

The `-update` flag is declared once per package:
`var update = flag.Bool("update", false, "update golden files")`.
Run `go test -update ./internal/dialer/...` after intentional output
changes; commit the regenerated `testdata/`.

## Other Test Categories

These categories sit outside the hot loop but are part of the suite:

- **Load.** k6 scripts (`tests/load/`) — 500 operators in WS,
  state-change every 5 s, asserting p95 event latency < 500 ms. SIPp
  scenarios for telephony — 200 concurrent SIP channels through the FS
  cluster. Run before each release; alarming on any p95 regression
  > 20%.
- **Chaos.** Chaos Mesh (`tests/chaos/`) on staging — kill one
  `cmd/api` pod, kill an FS-VM, network-partition Postgres. Asserts
  recovery semantics of `realtime` reconnect, recording lossiness, and
  503 vs 500 surface. Manual on green-build cadence.
- **Security.** SAST (gosec, eslint-plugin-security), dependency scan
  (govulncheck, osv-scanner, trivy, npm audit), DAST (OWASP ZAP
  weekly against staging). Penetration test annually for production.
  See `docs/security/` for the playbook.

## Linter Mapping

The mechanically enforced parts of this strategy:

| Rule | Linter |
|---|---|
| `t.Parallel()` mandatory | `paralleltest` |
| `t.Helper()` in helpers | `thelper` |
| `assert` / `require` / `expect` idioms | `testifylint` |
| `errors.Is/As` over equality | `errorlint` |
| `bodyclose` for `*http.Response` | `bodyclose` |
| `sqlclosecheck` for SQL rows | `sqlclosecheck` |
| `rowserrcheck` for rows.Err() | `rowserrcheck` |
| Comma-ok type assertions | `forcetypeassert` |
| Module isolation `internal/X/api/` only | `depguard:module-boundaries` |

## What this strategy does NOT yet cover (system-level gap)

The pyramid above is solid at the **per-module** level. What it does
not yet cover is the **system level** — full `cmd/api` boot against
the real backing stack, with cross-module flows exercised over the
public HTTP / WS surface. As of 2026-05-10:

- No `tests/smoke/` package exists yet (planned in
  `09-agent-workflow-improvements.md` § Improvement #5).
- No REST collection (Bruno / Postman) for manual exploration.
- No Frontend E2E (Playwright) — owned by `sociopulse-web` repo,
  Plan 15+.
- No real-FreeSWITCH integration — owned by Plan 08.
- No real-Yandex-SDK adapter coverage — owned by Plan 01.
- No chaos / load — pre-launch milestone.

The full gap analysis, the rationale ("not coverage — confidence"),
the failure-class examples, and the phased closure plan live in
**[`10-end-to-end-testing-gaps.md`](./10-end-to-end-testing-gaps.md)**.
Read that document before assuming "the system is tested" based on
the unit + integration numbers above.

## Cross-references

- `08-tdd-discipline.md` — the Red-Green-Refactor playbook.
- `02-module-contracts.md` — interfaces tests build mocks for.
- `06-observability.md` — metrics emitted on test failures (CI).
- `07-go-coding-standards.md` § Testing — the cc-skills heritage.
- `09-agent-workflow-improvements.md` — workflow-level testing
  improvements.
- `10-end-to-end-testing-gaps.md` — system-level gaps + closure plan.
- `samber/cc-skills-golang@golang-testing` — full skill at
  `~/.agents/skills/golang-testing/SKILL.md`.
- ADR-0015 — TDD as mandatory discipline.
- Spec §17 — the source pyramid and load/chaos scenarios.

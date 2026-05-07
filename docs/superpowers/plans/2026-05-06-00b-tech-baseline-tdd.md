# Tech Baseline (gin + zap) and TDD Discipline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lock the HTTP-router (gin) and structured-logger (zap) choices into ADRs, establish the project-wide TDD discipline document, and migrate every existing plan reference from `go-chi/chi` to `gin-gonic/gin` so subsequent plan execution starts with one consistent tech baseline.

**Architecture:** Document-only plan — no Go code is written here. Outputs are: two new ADRs (`docs/adr/0014-*.md`, `docs/adr/0015-*.md`), one new architecture doc (`docs/architecture/08-tdd-discipline.md`), per-line edits in 7 plan files where `chi` appears, an updated spec subsection §17.8, and an updated Plan 00a File Structure listing. After this plan, every downstream plan (02, 05, 11, 13, 14) talks about gin handlers, gin middleware, gin test mode — the engineer never sees a chi reference at execution time.

**Tech Stack (baseline locked here):**
- HTTP router/middleware: `github.com/gin-gonic/gin` v1.10+
- Structured logger: `go.uber.org/zap` v1.27+ (already in ADR-0012; reaffirmed here)
- Logger middleware bridge: `github.com/gin-contrib/zap` (gin → zap adapter, lets us keep zap as the structured backend per ADR-0012)
- Recovery middleware: `gin.Recovery()` (built-in)
- Test discipline: `testing` + `github.com/stretchr/testify` (helpers only) + `go.uber.org/goleak` + `github.com/testcontainers/testcontainers-go` (integration) — formalised in `docs/architecture/08-tdd-discipline.md`

**Spec sections covered:** §17 (test strategy), §17.8 (Go Coding Standards), §22 (ADR list).

**Prerequisites:**
- Plan 00 not yet executed — no Go code exists yet, so chi → gin migration is purely a doc rewrite.
- `samber/cc-skills-golang` skill pack readable at `~/.agents/skills/golang-*/`.

**What this plan does NOT do:**
- No Go code. No `go.mod` edits. No actual gin import. The first time gin appears in compiled code is Plan 02 Task 1.
- No tests run. The TDD-task scaffolds in this plan are *document-edit* tasks: "edit text → grep verifies edit → commit".

**Execution order:** runs **after Plan 00, after Plan 00a, before Plan 02**. The flow becomes Plan 00 → 00a → 00b → 02 → 03 → ... .

---

## Why gin over chi

Both are mature production routers. Reasons for picking gin in this project:

1. **First-class middleware ecosystem for our exact stack.** `gin-contrib/zap` is the canonical gin-zap bridge — it satisfies ADR-0012 (zap as structured logger) and §15 (observability conventions: tenant_id/request_id/trace_id in every log line) without us writing custom middleware.
2. **`c.Bind*` + `c.JSON` cuts boilerplate.** chi requires manual `json.NewDecoder(r.Body).Decode(&dto)` + `json.NewEncoder(w).Encode(resp)` in every handler. gin's `c.ShouldBindJSON(&dto)` + `c.JSON(http.StatusOK, resp)` halves the handler code.
3. **Built-in test mode (`gin.SetMode(gin.TestMode)`)** silences logs in tests deterministically. With chi we'd swap loggers manually per test.
4. **Familiarity** — gin is the de-facto Go HTTP framework in the Russian Go community. Onboarding new engineers is cheaper.
5. **`samber/cc-skills-golang@golang-grpc` § Common Mistakes** doesn't take a side; both routers satisfy the skill's design requirements (interceptors, graceful shutdown). Pick one and move on.

Trade-offs accepted: gin uses a custom `Context` rather than stdlib `http.Handler`, so middleware that's `http.Handler`-shaped needs an adapter. We document the adapter pattern in `pkg/httputil/gin_adapter.go` (Plan 00a Task 5 → updated by this plan).

## Why TDD discipline lives in its own document

The existing 22 plans are *task lists with TDD scaffolding*. The discipline of HOW to do TDD — what makes a good test name, when to use table-driven, when subtests, when goleak, when testcontainers — is identical across plans and should not be duplicated 22 times. We extract it once into `docs/architecture/08-tdd-discipline.md`. Plans then reference the discipline by section number.

---

## File Structure

This plan creates / modifies the following files. Every change is a textual edit; no new code modules.

```
sociopulse-platform/
├── docs/
│   ├── adr/                                                # added in Plan 00a Task 2; this plan adds 2 more
│   │   ├── 0014-gin-http-router.md                         # NEW (this plan, Task 1)
│   │   └── 0015-tdd-discipline.md                          # NEW (this plan, Task 2)
│   ├── architecture/
│   │   ├── 07-go-coding-standards.md                       # already in Plan 00a Task 1
│   │   └── 08-tdd-discipline.md                            # NEW (this plan, Task 3)
│   └── superpowers/
│       ├── specs/
│       │   └── 2026-05-06-sociopulse-system-design.md      # MODIFY §17.8 + §22
│       └── plans/
│           ├── 2026-05-06-00-foundation.md                 # MODIFY Task 14 doc index
│           ├── 2026-05-06-00a-architecture-foundation.md   # MODIFY Task 1 (file struct + step), Task 7 (Deps struct)
│           ├── 2026-05-06-00b-tech-baseline-tdd.md         # this plan
│           ├── 2026-05-06-02-cmd-api-skeleton.md           # MODIFY chi → gin everywhere
│           ├── 2026-05-06-05-auth-module.md                # MODIFY chi → gin in routes.go
│           ├── 2026-05-06-11-realtime-module.md            # MODIFY chi → gin in HTTP handlers
│           ├── 2026-05-06-13-analytics-reports.md          # MODIFY chi → gin in Endpoints()
│           └── 2026-05-06-14-billing-module.md             # MODIFY chi → gin in Endpoints()
└── CLAUDE.md                                                # MODIFY tech-stack note
```

**Note:** ADR files 0001-0013 are created in Plan 00a Task 2 (promotes spec §22 ADRs to standalone files). This plan adds ADR-0014 and ADR-0015 *to the spec §22 list and as standalone files*. Plan 00a Task 2 is updated implicitly — its loop "for each ADR in spec §22, create a standalone file" picks up the two new entries because we add them to spec §22 before Plan 00a Task 2 runs.

---

## Task 1 — ADR-0014: gin over chi for HTTP routing

**Files:**
- Create: `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` — modify §22 to add ADR-0014 entry
- Create: `docs/adr/0014-gin-http-router.md` (only after Plan 00a Task 2 has run; this task only edits the spec entry)

- [ ] **Step 1: Locate spec §22 ADR list end**

Run:
```bash
cd /Users/user/call-center/sociopulse-platform
grep -nE "^### ADR-001[0-3]" docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
```
Expected: 4 lines listing ADR-0010, ADR-0011, ADR-0012, ADR-0013 with line numbers. Note the line number of the last ADR-0013 entry — the new ADR-0014 is appended after its closing block.

- [ ] **Step 2: Read the ADR-0013 block end**

Read 30 lines starting at the ADR-0013 line found in Step 1 to identify the exact "Последствия" / "Consequences" closing paragraph. The next ADR is appended after this paragraph and before the `---` (or end of §22).

- [ ] **Step 3: Append ADR-0014 to spec §22**

In `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`, append after the last ADR-0013 line, before the `---` that closes §22:

```markdown
### ADR-0014. HTTP-роутер: gin-gonic/gin

**Status:** Accepted (2026-05-07).

**Context.** Backend cmd/api обслуживает REST API для оператора (~30 эндпоинтов) и админа (~80 эндпоинтов), плюс WebSocket-эндпоинты в realtime-модуле. Нужен HTTP-роутер с middleware-цепочкой: request-id, structured logging (zap), recovery, JWT-валидация, RBAC, rate-limit, idempotency, tenant context (`SET LOCAL app.tenant_id`).

Кандидаты: net/http+chi, gin-gonic/gin, fiber, echo. Чистый `net/http` отвергнут — нужна декларативная routing-tree-семантика и группа middleware.

**Decision.** Gin (`github.com/gin-gonic/gin` v1.10+).

**Rationale.**
1. Bridge `gin-contrib/zap` совмещает gin с zap-логером (ADR-0012), даёт нам стандартизованные поля `tenant_id`/`request_id`/`trace_id` бесплатно.
2. `c.ShouldBindJSON(&dto)` + `c.JSON(status, resp)` сокращает шаблонный код в каждом handler ~в 2 раза против stdlib-стиля.
3. `gin.SetMode(gin.TestMode)` детерминированно глушит логи в `httptest`-сценариях.
4. Большое community + стабильный API (v1 с 2017).
5. RU Go-сообщество знакомо с gin — onboarding дешевле.

**Alternatives considered.**
- **chi** — отлично совместим со stdlib `http.Handler`, но требует ручного JSON-encode/decode и custom logger middleware. Потенциальная экономия: 200-300 строк handler-кода × 110 эндпоинтов.
- **echo** — функционально сопоставим с gin, но ecosystem меньше.
- **fiber** — fasthttp под капотом, несовместимо с net/http middleware (наш TLS termination, идемпотентность, healthcheck-клиенты — все net/http-shaped).

**Consequences.**
- `pkg/httputil/gin_adapter.go` нужен для перевода stdlib `http.Handler`-middleware (idempotency, requestid) в `gin.HandlerFunc`. Адаптер реализуется в Plan 00a Task 5.
- WebSocket upgrade использует `c.Request` + `c.Writer` напрямую (gorilla/websocket совместим). См. Plan 11.
- Все handler-сигнатуры: `func (h *Handler) Method(c *gin.Context)`. Тесты на handler через `httptest.NewRecorder()` + `gin.CreateTestContext(...)`.
- Запрет добавления `chi` в `go.mod` — депгард rule в Plan 00 Task 9.
```

- [ ] **Step 4: Verify edit**

```bash
grep -A2 "^### ADR-0014" docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
```
Expected: ADR-0014 header + first lines of context.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
git commit -m "docs(adr): add ADR-0014 — gin-gonic/gin as HTTP router"
```

---

## Task 2 — ADR-0015: TDD as mandatory development discipline

**Files:**
- Modify: `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` — append ADR-0015 to §22

- [ ] **Step 1: Append ADR-0015 to spec §22**

In `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`, append after ADR-0014 (created in Task 1):

```markdown
### ADR-0015. Test-Driven Development as mandatory discipline

**Status:** Accepted (2026-05-07).

**Context.** План-проекта состоит из 22 implementation plans, каждый из которых разбит на ~10-15 задач. Каждая задача описана как Red-Green-Refactor цикл: «write failing test → run it fails → implement → run it passes → commit». Это TDD-структура.

Без формального утверждения TDD как обязательной дисциплины, исполнители планов могут «срезать» — написать код первым, тесты вторым, а то и пропустить. Это создаёт невидимый технический долг: tests-after-the-fact не покрывают edge cases, не ловят regression, не служат документацией поведения.

**Decision.** TDD обязателен для всего нового кода в `internal/` и `pkg/`. Допустимые исключения:
1. `cmd/<binary>/main.go` — composition root, тесты — smoke (запустить и проверить /healthz).
2. Migrations — schema validation через интеграционные тесты.
3. Generated code (mocks, proto-stubs).

**Rationale.**
1. Спека §17.1 уже описывает пирамиду тестов с ~2000 unit-тестов как целевое количество. TDD — единственный способ достичь этого без отставания.
2. `samber/cc-skills-golang@golang-testing` § Persona: «You write tests to constrain behavior, not to hit coverage targets.» — TDD естественно ведёт к тестам, фиксирующим поведение.
3. Coverage-таргеты (≥85% service, ≥70% store, ≥60% http/grpc) выполняются автоматически если задача написана как RGR-цикл.

**Alternatives considered.**
- **Tests-after** — быстрее в моменте, медленнее в долгосрочной перспективе. Регрессия = детектор tests-after.
- **No tests** — отвергнуто бизнес-требованием §17.

**Consequences.**
- Каждая задача в planах 00a, 02-19 — RGR-цикл. PR-template требует подтверждения RGR-дисциплины.
- `superpowers:test-driven-development` skill — обязательная sub-skill при subagent-driven-development.
- `paralleltest` + `thelper` + `testifylint` линтеры обязательны (см. Plan 00 Task 9).
- Когда test становится «не TDD» (e.g. характеризация легаси), автор отмечает `// characterization: pre-existing behaviour` — будет поводом для review.
- Coverage gate в CI: `make test-cover` падает при <70% общего покрытия (см. Plan 00 Task 11).
- TDD-методология распилована в `docs/architecture/08-tdd-discipline.md` (см. Task 3 этого плана).
```

- [ ] **Step 2: Verify**

```bash
grep -A2 "^### ADR-0015" docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
```
Expected: header + first lines.

- [ ] **Step 3: Commit**

```bash
git add docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
git commit -m "docs(adr): add ADR-0015 — TDD as mandatory discipline"
```

---

## Task 3 — `docs/architecture/08-tdd-discipline.md` (distilled golang-testing + golang-troubleshooting)

**Files:**
- Create: `docs/architecture/08-tdd-discipline.md`

- [ ] **Step 1: Create the document with full content**

Create `docs/architecture/08-tdd-discipline.md` with this content (verbatim):

```markdown
# 08. TDD Discipline

This document distils `samber/cc-skills-golang@golang-testing` and
`@golang-troubleshooting` into the project's TDD methodology. Every
implementation plan task is an instance of this discipline.

## The Red-Green-Refactor Loop

Every new behaviour goes through three phases:

1. **Red.** Write the smallest test that captures the new behaviour. Run it.
   Confirm it fails for the *right* reason — not a compile error or a typo,
   but the absence of the production code under test. If it doesn't fail,
   the test is wrong.
2. **Green.** Write the minimum production code to make the test pass. No
   speculative generalization, no extra branches "just in case".
3. **Refactor.** With tests as a safety net, improve names, extract
   functions, deduplicate. Run tests after every refactor step.

Each plan task in this project corresponds to **one Red-Green-Refactor
cycle**. The 5 steps inside a task (write test → run fails → implement →
run passes → commit) ARE the cycle.

## What Makes a Good Test

A good test is:

1. **Named after the behaviour, not the function.**
   - Bad: `TestCalculatePrice`
   - Good: `TestCalculatePrice_BulkDiscountAt100Items_Reduces10Percent`

   Subtest names follow the same rule: `t.Run("zero quantity returns zero", ...)`.

2. **Independent.** A test must pass when run alone or with all others. No
   ordering dependencies, no shared mutable state. If a test relies on
   another test's side effect, the suite is broken.

3. **Fast.** Unit tests target <1 ms each. Integration tests use
   `//go:build integration` build tag and are skipped in the default
   `go test` run.

4. **Deterministic.** No clock dependence (use `clockwork`), no randomness
   (seed `math/rand/v2.New(rand.NewPCG(seed, seed))` from a fixed seed in
   tests; never use `crypto/rand` in tests where determinism matters).

5. **Single-purpose.** One assertion per concept. A test that checks 5
   independent properties produces 5 confusing failure messages instead of
   1 clear one.

## Table-Driven Tests with Subtests

The default shape for testing multiple inputs:

```go
func TestCalculatePrice(t *testing.T) {
    t.Parallel() // outer test parallel with siblings

    tests := []struct {
        name      string
        quantity  int
        unitPrice float64
        want      float64
        wantErr   error
    }{
        {"single item", 1, 10.0, 10.0, nil},
        {"bulk discount at 100 items", 100, 10.0, 900.0, nil},
        {"zero quantity returns zero", 0, 10.0, 0.0, nil},
        {"negative quantity rejected", -1, 10.0, 0.0, ErrNegativeQuantity},
    }

    for _, tt := range tests {
        tt := tt // capture for go1.21- ; harmless on 1.22+
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel() // subtests parallel within the table

            got, err := CalculatePrice(tt.quantity, tt.unitPrice)
            if !errors.Is(err, tt.wantErr) {
                t.Fatalf("CalculatePrice(%d, %.2f): err = %v, want %v",
                    tt.quantity, tt.unitPrice, err, tt.wantErr)
            }
            if got != tt.want {
                t.Errorf("CalculatePrice(%d, %.2f) = %.2f, want %.2f",
                    tt.quantity, tt.unitPrice, got, tt.want)
            }
        })
    }
}
```

Key invariants enforced by linters (`paralleltest`, `thelper`,
`testifylint` — see Plan 00 Task 9):
- Every `*testing.T`-rooted test calls `t.Parallel()` once at the top.
- Subtests have unique names so `go test -run TestX/<name>` works.
- Test helpers call `t.Helper()` before `t.Errorf`/`t.Fatalf`.

## Testify: Helper, Not Replacement

We use `github.com/stretchr/testify/require` and `.../assert` as helpers,
not as a wrapper around the standard `testing` package. Rules:

1. **`require.X` for preconditions** that, if violated, make the rest of
   the test meaningless (`require.NoError(t, err)` after a `New(...)` call).
2. **`assert.X` for individual property checks** within a single test.
3. **`testify/suite` is forbidden.** It hides the test boundary, breaks
   `t.Parallel()` at subtest level, and conflicts with goleak.
4. **`testify/mock` is forbidden** — we use `mockery`-generated mocks
   from `internal/<module>/api/` interfaces. Hand-rolled mocks via
   `testify/mock` are fragile and unmaintainable.

Failure messages from `assert`/`require` already include the file:line and
the operands; do NOT add `assert.Equal(t, x, y, "x should equal y")` —
the message is redundant noise.

## Goroutine Leak Detection

Every package that spawns a goroutine in production code MUST install
`go.uber.org/goleak.VerifyTestMain` in a `main_test.go`:

```go
// internal/dialer/service/main_test.go
package service

import (
    "testing"

    "go.uber.org/goleak"
)

func TestMain(m *testing.M) {
    goleak.VerifyTestMain(m)
}
```

If a known goroutine leak exists (e.g., a third-party library that doesn't
close), exclude it explicitly:

```go
goleak.VerifyTestMain(m,
    goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
)
```

Never use `goleak.IgnoreCurrent()` — it ignores ALL goroutines alive at
test start, masking real leaks introduced by the test under development.

## Race Detector

Every CI run uses `go test -race -count=1 ./...`. The `-count=1` defeats
the test cache so the race detector actually runs. Local development can
omit `-count=1` for speed; CI must include it.

A race detector finding is **never** a flaky test. It's a real bug. Fix
the race; never `// nolint:race` it.

## Integration Tests with testcontainers-go

Integration tests live next to unit tests but use `//go:build integration`:

```go
//go:build integration

package store_test

import (
    "context"
    "testing"

    "github.com/jackc/pgx/v5"
    "github.com/testcontainers/testcontainers-go/modules/postgres"
    "github.com/stretchr/testify/require"
)

func TestUserStore_CreateUser_PersistsRow(t *testing.T) {
    t.Parallel()

    ctx := context.Background()
    pgC, err := postgres.Run(ctx, "postgres:16",
        postgres.WithDatabase("sociopulse_test"),
        postgres.WithUsername("test"),
        postgres.WithPassword("test"),
    )
    require.NoError(t, err)
    t.Cleanup(func() { _ = pgC.Terminate(ctx) })

    dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
    require.NoError(t, err)

    conn, err := pgx.Connect(ctx, dsn)
    require.NoError(t, err)
    t.Cleanup(func() { _ = conn.Close(ctx) })

    // ... migration + actual test
}
```

Run integration tests separately:

```bash
go test -tags=integration -race -count=1 ./...
```

CI runs integration tests on PR; main-branch CI runs both unit and
integration.

## Gin-specific HTTP Test Pattern (per ADR-0014)

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
)

func TestLoginHandler_BadCredentials_Returns401(t *testing.T) {
    t.Parallel()

    gin.SetMode(gin.TestMode) // silences gin's default logger

    svc := &fakeAuthService{loginErr: api.ErrInvalidCredentials}
    h := NewHandler(svc)

    body, err := json.Marshal(api.LoginRequest{
        Email: "alice@example.com", Password: "wrong",
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

`gin.SetMode(gin.TestMode)` is the FIRST line of every test file's
`TestMain` (or the first line of every test function if no `TestMain`).
This silences gin's default logger so the test output stays readable.

For unit-testing a handler in isolation (without routing), use
`gin.CreateTestContext`:

```go
rec := httptest.NewRecorder()
c, _ := gin.CreateTestContext(rec)
c.Request = httptest.NewRequest(http.MethodGet, "/x?foo=bar", nil)
c.Params = gin.Params{{Key: "id", Value: "42"}}

h.GetUser(c) // call handler directly

assert.Equal(t, http.StatusOK, rec.Code)
```

## Fixtures and Golden Files

For complex outputs (rendered XML dialplan, JSON survey schema), use
`testdata/` golden files:

```go
got, err := dialplan.Render(ctx, project)
require.NoError(t, err)

golden := filepath.Join("testdata", "dialplan_simple.xml")
if *update {
    require.NoError(t, os.WriteFile(golden, got, 0644))
}

want, err := os.ReadFile(golden)
require.NoError(t, err)
assert.Equal(t, string(want), string(got))
```

The `-update` flag lives in `var update = flag.Bool("update", false, "update golden files")` at the top of the test file. Run `go test -update ./internal/dialer/...` after intentional output changes; commit the regenerated `testdata/`.

## Test-Driven Debugging (golang-troubleshooting)

When a bug is reported (production or local):

1. **Reproduce first.** Write a failing test that captures the bug. If you
   cannot reproduce, you cannot fix.
2. **Read the error message.** Stack trace, line number, variable name —
   read it fully before forming a hypothesis.
3. **One hypothesis at a time.** Change one variable, re-run, observe.
4. **Fix the root cause.** Symptom-fixes ("add a nil check") that don't
   explain WHY the bug happens are forbidden. The PR description must
   include "Root cause: ..." sentence.

The new test stays in the suite as a regression guard.

## Coverage Targets

Per spec §17.2 and `docs/architecture/04-testing-strategy.md`:

| Layer | Target |
|---|---|
| `internal/<module>/service/` | ≥ 85% |
| `internal/<module>/store/` | ≥ 70% |
| `internal/<module>/http/` (gin handlers) | ≥ 60% |
| `internal/<module>/grpc/` | ≥ 60% |
| `pkg/*` | ≥ 80% |
| `cmd/<binary>/main.go` | smoke only — `/healthz` returns 200 |

CI gate: `make test-cover` fails if total project coverage < 70%.

## Cross-references

- `samber/cc-skills-golang@golang-testing` — full skill, source of this discipline.
- `samber/cc-skills-golang@golang-troubleshooting` — TDD for debugging.
- `docs/architecture/07-go-coding-standards.md` — broader Go standards.
- `docs/architecture/04-testing-strategy.md` — pyramid, tools, scenarios.
- ADR-0015 — TDD as mandatory discipline.

```

- [ ] **Step 2: Verify file**

```bash
wc -l docs/architecture/08-tdd-discipline.md
grep -c "^## " docs/architecture/08-tdd-discipline.md
```
Expected: ≥ 250 lines, ≥ 10 H2 sections.

- [ ] **Step 3: Commit**

```bash
git add docs/architecture/08-tdd-discipline.md
git commit -m "docs(architecture): add 08-tdd-discipline.md (Red-Green-Refactor + gin/goleak/testcontainers patterns)"
```

---

## Task 4 — Update Plan 00a Task 1 to include the new doc + Task 7 chi → gin

**Files:**
- Modify: `docs/superpowers/plans/2026-05-06-00a-architecture-foundation.md`

- [ ] **Step 1: Update Task 1 File Structure listing**

Find the line:
```
│   │   └── 07-go-coding-standards.md                 # samber/cc-skills-golang distilled
```

Replace with:
```
│   │   ├── 07-go-coding-standards.md                 # samber/cc-skills-golang distilled
│   │   └── 08-tdd-discipline.md                      # Red-Green-Refactor methodology (Plan 00b)
```

Verify:
```bash
grep "08-tdd-discipline" docs/superpowers/plans/2026-05-06-00a-architecture-foundation.md
```
Expected: 1 line in File Structure section.

- [ ] **Step 2: Update Task 1 Files block**

Find the block of `- Create:` lines for architecture docs in Task 1. Currently ends with:
```
- Create: `docs/architecture/07-go-coding-standards.md`
```

Add a comment line so Plan 00a executor knows 08 lives in Plan 00b:
```
- Create: `docs/architecture/07-go-coding-standards.md`
- (Note: `08-tdd-discipline.md` is created by Plan 00b — execute Plan 00b before Plan 02)
```

Verify:
```bash
grep "08-tdd-discipline.md is created by Plan 00b" docs/superpowers/plans/2026-05-06-00a-architecture-foundation.md
```
Expected: 1 match.

- [ ] **Step 3: Update Task 7 Module Deps struct chi → gin**

Find the line in Task 7:
```go
    HTTPRouter   chi.Router
```

Replace with:
```go
    HTTPRouter   *gin.Engine
```

And find the import line:
```go
    "github.com/go-chi/chi/v5"
```

Replace with:
```go
    "github.com/gin-gonic/gin"
```

Verify both:
```bash
grep -E "chi\.Router|go-chi/chi" docs/superpowers/plans/2026-05-06-00a-architecture-foundation.md
```
Expected: zero matches.

```bash
grep -E "gin\.Engine|gin-gonic/gin" docs/superpowers/plans/2026-05-06-00a-architecture-foundation.md
```
Expected: 2 matches (Deps struct + import).

- [ ] **Step 4: Update Task 5 pkg/httputil description**

Find Step 7 of Task 5 (currently mentions "stub helper functions ... IdempotencyMiddleware, RateLimitMiddleware. Plan 02 fills."). Replace the Step 7 paragraph with:

```markdown
- [ ] **Step 7: `pkg/grpc/` and `pkg/httputil/`**

Stub helper functions:
- `pkg/grpc/`: `NewMTLSServer`, `NewMTLSClient` — Plan 02 fills.
- `pkg/httputil/`: `RequestIDMiddleware`, `IdempotencyMiddleware`, `RateLimitMiddleware`,
  `RecoveryMiddleware` — Plan 02 fills. **All return `gin.HandlerFunc` per ADR-0014.**
- `pkg/httputil/gin_adapter.go`: `FromHTTPHandler(http.Handler) gin.HandlerFunc` — adapter
  for stdlib middleware. Plan 02 implements; here just declare the function signature
  with `panic("not implemented: see Plan 02")`.

Why an adapter exists: some middleware (TLS termination, raw HTTP healthcheck) is
shaped as stdlib `http.Handler`. The adapter wraps such middleware to fit gin's
`HandlerFunc` chain so we can mix-and-match without rewriting stable middleware.
```

Verify:
```bash
grep "FromHTTPHandler" docs/superpowers/plans/2026-05-06-00a-architecture-foundation.md
```
Expected: 1 match.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/plans/2026-05-06-00a-architecture-foundation.md
git commit -m "docs(plan-00a): chi→gin in Module Deps; reference 08-tdd-discipline.md"
```

---

## Task 5 — Update Plan 02 (cmd/api skeleton) chi → gin

**Files:**
- Modify: `docs/superpowers/plans/2026-05-06-02-cmd-api-skeleton.md`

- [ ] **Step 1: Locate chi imports + types in Plan 02**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-02-cmd-api-skeleton.md
```
Expected: ≥ 5 matches showing import lines, `chi.NewRouter()`, `chi.Router`, etc.

- [ ] **Step 2: Replace each chi reference**

For each match found in Step 1, apply the corresponding replacement. The mapping is:

| chi pattern | gin replacement |
|---|---|
| `"github.com/go-chi/chi/v5"` | `"github.com/gin-gonic/gin"` |
| `"github.com/go-chi/chi/v5/middleware"` | `// gin built-in middleware: gin.Logger(), gin.Recovery()` |
| `r := chi.NewRouter()` | `r := gin.New()` (then `r.Use(gin.Recovery())` for safety) |
| `chi.Router` (parameter type) | `*gin.Engine` (top-level) or `*gin.RouterGroup` (nested) |
| `r.Get("/path", h)` | `r.GET("/path", h)` |
| `r.Post("/path", h)` | `r.POST("/path", h)` |
| `r.Route("/api/x", fn)` | `apiX := r.Group("/api/x"); fn(apiX)` |
| `r.Use(middleware.Logger)` | `r.Use(ginzap.Ginzap(logger, time.RFC3339, true))` |
| `r.Use(middleware.Recoverer)` | `r.Use(gin.Recovery())` |
| `r.Use(middleware.RequestID)` | `r.Use(httputil.RequestIDMiddleware())` (custom, returns `gin.HandlerFunc`) |
| handler signature `func(w http.ResponseWriter, r *http.Request)` | `func(c *gin.Context)` |
| `chi.URLParam(r, "id")` | `c.Param("id")` |
| `r.URL.Query().Get("q")` | `c.Query("q")` (or `c.DefaultQuery("q", "")`) |
| `json.NewDecoder(r.Body).Decode(&dto)` | `if err := c.ShouldBindJSON(&dto); err != nil { ... }` |
| `w.WriteHeader(http.StatusXxx); json.NewEncoder(w).Encode(resp)` | `c.JSON(http.StatusXxx, resp)` |

Apply ALL replacements. Each Edit operation is a separate edit — do not bundle.

- [ ] **Step 3: Add gin-contrib/zap as a Plan 02 dependency**

Find the Tech Stack section at the top of Plan 02. Currently lists `github.com/go-chi/chi/v5 v5.0+, github.com/go-chi/chi/v5/middleware`. Replace with:

```
- `github.com/gin-gonic/gin` v1.10+ (HTTP router, ADR-0014)
- `github.com/gin-contrib/zap` (zap-bridge for gin Logger middleware, satisfies ADR-0012)
- `go.uber.org/zap` v1.27+ (structured logger, ADR-0012)
- `pkg/httputil` (gin-adapter for stdlib middleware, see Plan 00a Task 5)
```

- [ ] **Step 4: Verify zero chi remaining in Plan 02**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-02-cmd-api-skeleton.md
```
Expected: zero matches.

```bash
grep -nE "gin\." docs/superpowers/plans/2026-05-06-02-cmd-api-skeleton.md | wc -l
```
Expected: ≥ 5 matches.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/plans/2026-05-06-02-cmd-api-skeleton.md
git commit -m "docs(plan-02): migrate chi→gin per ADR-0014; add gin-contrib/zap"
```

---

## Task 6 — Update Plan 05 (auth-module) chi → gin

**Files:**
- Modify: `docs/superpowers/plans/2026-05-06-05-auth-module.md`

- [ ] **Step 1: Locate chi mentions**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-05-auth-module.md
```
Expected: ≥ 2 matches (Tech Stack list + `Mount(r chi.Router, deps)` signature).

- [ ] **Step 2: Apply replacements**

Use the mapping table from Task 5 Step 2. Specifically:
- Tech Stack line `- github.com/go-chi/chi/v5 (mounted in Plan 02) — HTTP routing.` → replace with `- github.com/gin-gonic/gin v1.10+ (ADR-0014; mounted in Plan 02).`
- `Mount(r chi.Router, deps)` → `Mount(r *gin.RouterGroup, deps Deps)` (handlers attach to a route group provided by cmd/api)
- All `r.Get(...)`, `r.Post(...)`, etc. inside the auth-module routes → `r.GET(...)`, `r.POST(...)`, etc.
- All handler signatures → `func(c *gin.Context)`
- All `chi.URLParam(r, "id")` → `c.Param("id")`

- [ ] **Step 3: Verify**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-05-auth-module.md
```
Expected: zero matches.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-05-06-05-auth-module.md
git commit -m "docs(plan-05): migrate chi→gin per ADR-0014"
```

---

## Task 7 — Update Plan 11 (realtime-module) chi → gin

**Files:**
- Modify: `docs/superpowers/plans/2026-05-06-11-realtime-module.md`

- [ ] **Step 1: Locate chi mentions**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-11-realtime-module.md
```
Expected: ≥ 5 matches (HTTP routes for listen-in commands).

- [ ] **Step 2: Apply replacements**

Use the mapping table from Task 5 Step 2. Realtime-module has:
- HTTP listen-in command endpoints (`POST /api/calls/{id}/listen-in`)
- Operator-control endpoints (`POST /api/operators/{id}/pause`, etc.)
- Each handler currently uses `chi.URLParam(r, "id")` — replace with `c.Param("id")`.

Pay special attention: realtime also has WebSocket endpoints. For WS, the handler signature stays as a plain http.Handler that we wrap into gin via `pkg/httputil.FromHTTPHandler` (gorilla/websocket-style upgrade). Add the wrapper invocation example:

```go
r.GET("/ws/operator", httputil.FromHTTPHandler(wsHandler))
```

- [ ] **Step 3: Verify**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-11-realtime-module.md
```
Expected: zero matches.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-05-06-11-realtime-module.md
git commit -m "docs(plan-11): migrate chi→gin per ADR-0014; WS via FromHTTPHandler adapter"
```

---

## Task 8 — Update Plan 13 (analytics-reports) chi → gin

**Files:**
- Modify: `docs/superpowers/plans/2026-05-06-13-analytics-reports.md`

- [ ] **Step 1: Locate chi mentions**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-13-analytics-reports.md
```
Expected: ≥ 2 matches (`Endpoints(svc ServiceRO, ...) chi.Router`, `r := chi.NewRouter()`).

- [ ] **Step 2: Apply replacements**

The pattern in Plan 13 is:
```go
func Endpoints(svc ServiceRO, requireAdmin func(http.Handler) http.Handler) chi.Router {
    r := chi.NewRouter()
    // ...
    return r
}
```

Replace with:
```go
func Endpoints(svc ServiceRO, requireAdmin gin.HandlerFunc) func(*gin.RouterGroup) {
    return func(r *gin.RouterGroup) {
        r.Use(requireAdmin)
        // ... routes
    }
}
```

The caller (cmd/api) does:
```go
adminGroup := router.Group("/api/admin")
analyticsHTTP.Endpoints(svc, requireAdminMW)(adminGroup)
```

This shape avoids leaking gin.Engine vs gin.RouterGroup confusion.

- [ ] **Step 3: Verify**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-13-analytics-reports.md
```
Expected: zero matches.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-05-06-13-analytics-reports.md
git commit -m "docs(plan-13): migrate chi→gin per ADR-0014; Endpoints returns RouterGroup-mounter"
```

---

## Task 9 — Update Plan 14 (billing-module) chi → gin

**Files:**
- Modify: `docs/superpowers/plans/2026-05-06-14-billing-module.md`

- [ ] **Step 1: Locate chi mentions**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-14-billing-module.md
```
Expected: ≥ 5 matches (Endpoints with finance/billing routes).

- [ ] **Step 2: Apply replacements**

Same pattern as Plan 13 (Task 8 Step 2). Billing has two route groups: `/api/finance` and `/api/billing/tariffs`. Each becomes:

```go
func (e Endpoints) Mount(r *gin.RouterGroup, mw struct{ Admin gin.HandlerFunc }) {
    finance := r.Group("/api/finance")
    finance.Use(mw.Admin)
    // ... finance routes (GET tariffs, GET my-bill, etc.)

    tariffs := r.Group("/api/billing/tariffs")
    tariffs.Use(mw.Admin)
    // ... tariff CRUD
}
```

Test handler example for billing GET endpoint:

```go
func (h *Handler) GetTariff(c *gin.Context) {
    tariffID := c.Param("id")
    tariff, err := h.svc.GetTariff(c.Request.Context(), tariffID)
    if err != nil {
        if errors.Is(err, api.ErrTariffNotFound) {
            c.JSON(http.StatusNotFound, errorEnvelope("billing.tariff_not_found", err))
            return
        }
        c.JSON(http.StatusInternalServerError, errorEnvelope("billing.internal", err))
        return
    }
    c.JSON(http.StatusOK, tariff)
}
```

- [ ] **Step 3: Verify**

```bash
grep -nE "chi|go-chi" docs/superpowers/plans/2026-05-06-14-billing-module.md
```
Expected: zero matches.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-05-06-14-billing-module.md
git commit -m "docs(plan-14): migrate chi→gin per ADR-0014"
```

---

## Task 10 — Update spec §17.8 with gin/HTTP testing patterns

**Files:**
- Modify: `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`

- [ ] **Step 1: Locate §17.8 end**

```bash
grep -nE "^### 17\.8|^## 18\." docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
```
Note both line numbers.

- [ ] **Step 2: Append HTTP testing subsection within §17.8**

Find the existing §17.8 last bullet (about ADR-0014 candidate). Insert before that bullet:

```markdown

**HTTP testing pattern (gin, per ADR-0014):**

```go
gin.SetMode(gin.TestMode) // FIRST line of test file's TestMain or test func

r := gin.New()
r.POST("/api/auth/login", h.Login)

req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
    strings.NewReader(`{"email":"x","password":"y"}`))
req.Header.Set("Content-Type", "application/json")
rec := httptest.NewRecorder()
r.ServeHTTP(rec, req)

assert.Equal(t, http.StatusXxx, rec.Code)
```

For handler-only unit tests (no routing), use `gin.CreateTestContext`:

```go
rec := httptest.NewRecorder()
c, _ := gin.CreateTestContext(rec)
c.Request = httptest.NewRequest(...)
c.Params = gin.Params{{Key: "id", Value: "42"}}
h.GetUser(c)
```

Full Red-Green-Refactor playbook lives in
[`docs/architecture/08-tdd-discipline.md`](../../architecture/08-tdd-discipline.md).
ADR-0015 makes TDD mandatory; ADR-0014 fixes the router choice; this section
is the testing surface where they meet.
```

Note: the candidate ADR-0014 bullet at the end of §17.8 (which previously talked about a "future zap → slog migration ADR-0014") is now stale because ADR-0014 has been allocated to gin. **Renumber the candidate to ADR-0016**:

Find:
```
**ADR-0014 candidate** (открыт): когда совершим miграцию `zap → slog`
(ADR-0012 текущий), `loggercheck.zap` будет заменён на `loggercheck.slog`
single-mode и `zap` уйдёт из allow-list импортов.
```

Replace with:
```
**ADR-0016 candidate** (открыт): когда совершим миграцию `zap → slog`
(ADR-0012 текущий), `loggercheck.zap` будет заменён на `loggercheck.slog`
single-mode и `zap` уйдёт из allow-list импортов.
```

- [ ] **Step 3: Verify**

```bash
grep -A1 "HTTP testing pattern (gin" docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
grep "ADR-0016 candidate" docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
```
Expected: both grep find their pattern.

```bash
grep "ADR-0014 candidate" docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
```
Expected: zero matches (renamed to ADR-0016).

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
git commit -m "docs(spec): §17.8 — add gin testing pattern; renumber stale candidate ADR-0014→ADR-0016"
```

---

## Task 11 — Update Plan 00 (foundation) doc index + CONTRIBUTING reference

**Files:**
- Modify: `docs/superpowers/plans/2026-05-06-00-foundation.md`

- [ ] **Step 1: Update doc-tree index in Task 14**

Find the line in Task 14 doc-tree:
```
- 00a — Architecture Foundation (module API contracts, pkg/ scaffolds, depguard, `docs/architecture/00–07-*.md`)
```

Replace with:
```
- 00a — Architecture Foundation (module API contracts, pkg/ scaffolds, depguard, `docs/architecture/00–07-*.md`)
- 00b — Tech Baseline & TDD Discipline (gin over chi, zap reaffirmed, `docs/architecture/08-tdd-discipline.md`)
```

- [ ] **Step 2: Update CONTRIBUTING.md (Task 5) Go Coding Standards subsection**

Find the section "## Go Coding Standards" added in earlier Plan 00 modifications. Locate the bullet about testing (item 8 — "Testing — table-driven..."). After it, append a new item:

```markdown
9. **HTTP — gin** (ADR-0014). Handlers are `func(c *gin.Context)`. JSON binding
   via `c.ShouldBindJSON(&dto)`, JSON output via `c.JSON(status, resp)`. URL
   params via `c.Param("id")`, query via `c.Query("q")`. Tests use
   `gin.SetMode(gin.TestMode)` + `httptest.NewRecorder()`; see
   [`docs/architecture/08-tdd-discipline.md`](docs/architecture/08-tdd-discipline.md)
   § "Gin-specific HTTP Test Pattern".
```

Also update the heading paragraph to mention 08-tdd-discipline.md alongside 07:

Find:
```
The full distilled standard for this project lives in
[`docs/architecture/07-go-coding-standards.md`](docs/architecture/07-go-coding-standards.md).
```

Replace with:
```
The full distilled standard for this project lives in
[`docs/architecture/07-go-coding-standards.md`](docs/architecture/07-go-coding-standards.md)
(coding standards) and
[`docs/architecture/08-tdd-discipline.md`](docs/architecture/08-tdd-discipline.md)
(test-driven development discipline + gin testing patterns).
```

- [ ] **Step 3: Verify both edits**

```bash
grep "00b — Tech Baseline" docs/superpowers/plans/2026-05-06-00-foundation.md
grep "HTTP — gin.*ADR-0014" docs/superpowers/plans/2026-05-06-00-foundation.md
grep "08-tdd-discipline.md" docs/superpowers/plans/2026-05-06-00-foundation.md
```
Expected: 3 matches across the 3 greps.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-05-06-00-foundation.md
git commit -m "docs(plan-00): index Plan 00b; add HTTP-gin item to CONTRIBUTING Go Standards"
```

---

## Task 12 — Update Plan 00 Task 9 .golangci.yml — depguard for chi ban

**Files:**
- Modify: `docs/superpowers/plans/2026-05-06-00-foundation.md`

- [ ] **Step 1: Find depguard `banned-stdlib` block in Task 9**

The block currently bans `math/rand`, MD5, SHA1, etc. We add a sibling `banned-third-party` rule for chi.

```bash
grep -n "banned-stdlib:" docs/superpowers/plans/2026-05-06-00-foundation.md
```
Note line number.

- [ ] **Step 2: Append `banned-third-party` rule**

In `.golangci.yml` block of Task 9, after the `banned-stdlib` rule, add:

```yaml
      # ADR-0014 — gin is the project router. Forbid alternatives
      # so a careless go-get cannot pull a competing router into go.mod.
      banned-third-party:
        list-mode: lax
        deny:
          - pkg: "github.com/go-chi/chi"
            desc: "use github.com/gin-gonic/gin per ADR-0014"
          - pkg: "github.com/go-chi/chi/v5"
            desc: "use github.com/gin-gonic/gin per ADR-0014"
          - pkg: "github.com/labstack/echo"
            desc: "use github.com/gin-gonic/gin per ADR-0014"
          - pkg: "github.com/gofiber/fiber"
            desc: "use github.com/gin-gonic/gin per ADR-0014"
```

- [ ] **Step 3: Verify**

```bash
grep "banned-third-party" docs/superpowers/plans/2026-05-06-00-foundation.md
```
Expected: 1 match.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/plans/2026-05-06-00-foundation.md
git commit -m "docs(plan-00): depguard banned-third-party — forbid chi/echo/fiber per ADR-0014"
```

---

## Task 13 — Update CLAUDE.md tech-stack note

**Files:**
- Modify: `/Users/user/call-center/sociopulse-platform/CLAUDE.md`

- [ ] **Step 1: Append tech baseline section**

Append at the end of CLAUDE.md:

```markdown

## Tech baseline (locked by ADRs)

- **HTTP router**: `github.com/gin-gonic/gin` v1.10+ (ADR-0014). Handlers
  are `func(c *gin.Context)`. JSON binding via `c.ShouldBindJSON`. Test
  mode via `gin.SetMode(gin.TestMode)`.
- **Logger**: `go.uber.org/zap` v1.27+ (ADR-0012). gin↔zap bridge:
  `github.com/gin-contrib/zap`.
- **Testing**: stdlib `testing` + `stretchr/testify` (helpers only) +
  `go.uber.org/goleak` + `testcontainers/testcontainers-go`. TDD is
  mandatory (ADR-0015). Methodology: `docs/architecture/08-tdd-discipline.md`.
- **Linters**: `.golangci.yml` enforces all of the above mechanically.
  See `docs/architecture/07-go-coding-standards.md` § Linter Mapping.
```

- [ ] **Step 2: Verify**

```bash
grep "Tech baseline" /Users/user/call-center/sociopulse-platform/CLAUDE.md
```
Expected: 1 match.

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md
git commit -m "docs(claude): tech baseline section (gin/zap/testify+goleak+testcontainers)"
```

---

## Task 14 — Final verification — every plan is gin-clean and references TDD discipline

**Files:** none (verification only)

- [ ] **Step 1: Whole-repo chi audit**

```bash
cd /Users/user/call-center/sociopulse-platform
grep -rnE "chi\.|go-chi" docs/ 2>/dev/null
```
Expected: zero matches across all `docs/`.

- [ ] **Step 2: gin presence audit**

```bash
grep -rnE "gin-gonic|gin\.Engine|gin\.Context|gin\.RouterGroup|gin\.HandlerFunc" docs/ 2>/dev/null | wc -l
```
Expected: ≥ 30 matches across plans.

- [ ] **Step 3: TDD discipline cross-references**

```bash
grep -rln "08-tdd-discipline.md" docs/
```
Expected: ≥ 5 files (Plan 00, Plan 00a, Plan 00b, spec, CLAUDE.md).

- [ ] **Step 4: ADR-0014/0015 visible in spec**

```bash
grep -E "^### ADR-001[45]" docs/superpowers/specs/2026-05-06-sociopulse-system-design.md
```
Expected: 2 matches.

- [ ] **Step 5: Tag the milestone**

```bash
git tag -a v0.0.2-tech-baseline -m "Plan 00b complete: gin/zap/TDD baseline locked"
git tag -l
```
Expected: `v0.0.2-tech-baseline` listed.

- [ ] **Step 6: Push**

```bash
git push origin main
git push origin v0.0.2-tech-baseline
```

---

## Self-review

**1. Spec coverage:**

| Requirement | Task |
|---|---|
| ADR for gin choice | Task 1 (ADR-0014 in spec §22) |
| ADR for TDD discipline | Task 2 (ADR-0015 in spec §22) |
| TDD methodology document | Task 3 (`docs/architecture/08-tdd-discipline.md`) |
| Plan 00a uses gin | Task 4 (Module Deps + pkg/httputil) |
| Plan 02 uses gin | Task 5 |
| Plan 05 uses gin | Task 6 |
| Plan 11 uses gin | Task 7 |
| Plan 13 uses gin | Task 8 |
| Plan 14 uses gin | Task 9 |
| Spec §17.8 has gin testing pattern | Task 10 |
| CONTRIBUTING.md mentions gin/TDD doc | Task 11 |
| `.golangci.yml` bans non-gin routers | Task 12 |
| CLAUDE.md tech baseline section | Task 13 |
| End-to-end audit | Task 14 |

No requirement uncovered.

**2. Placeholder scan:** none of "TBD", "implement later", "similar to Task N" appear. Each task contains the exact textual edit.

**3. Type consistency:**
- ADR numbering: 0014 (gin), 0015 (TDD), 0016 (zap→slog future). Renamed in Task 10 Step 2. Consistent.
- Module Deps `HTTPRouter` field: `*gin.Engine` everywhere (Task 4 Step 3, Task 5 Step 2 implicit via cmd/api). Consistent.
- Mount signatures: `Mount(r *gin.RouterGroup, ...)` for all per-module router-mounters (Tasks 6, 8, 9). Consistent.

**4. Edge cases handled:**
- WebSocket (Plan 11) — uses `pkg/httputil.FromHTTPHandler` adapter (Task 7 Step 2).
- stdlib middleware compatibility — `pkg/httputil/gin_adapter.go` provides `FromHTTPHandler` (Task 4 Step 4).
- gin's default logger is silenced with `gin.SetMode(gin.TestMode)` in tests (Task 3 doc, Task 10 spec section).

Plan complete. Saved to `docs/superpowers/plans/2026-05-06-00b-tech-baseline-tdd.md`.

---

## Execution choice

Plan complete and saved to `docs/superpowers/plans/2026-05-06-00b-tech-baseline-tdd.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration. Each task is a small textual edit; subagent reviews are quick.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?

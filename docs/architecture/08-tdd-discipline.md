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

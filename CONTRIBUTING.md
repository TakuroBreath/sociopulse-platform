# Contributing

## Development workflow

1. Branch from `main`: `git checkout -b feat/<short-description>` (or `fix/`, `chore/`, `refactor/`, `docs/`).
2. Write tests first (TDD per ADR-0015). All new code must have tests.
3. Run `make lint test` before pushing.
4. Open a PR. CI must pass. Get one approving review.
5. Squash-merge to `main`.

## Commit messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

- `feat:` new feature
- `fix:` bug fix
- `chore:` tooling, config, no code change
- `refactor:` code change without behavior change
- `test:` test-only changes
- `docs:` documentation only
- `perf:` performance improvement
- `build:` build/CI change

Scope optional but encouraged: `feat(dialer): add progressive ratio support`.

## Code style

- Go: `gofmt`, `goimports`, `golangci-lint` enforced via `.golangci.yml`. Tab indents.
- TypeScript: `eslint`, `prettier`. 2-space indents.
- SQL: lower-case keywords, snake_case identifiers.
- YAML: 2-space indents, strings double-quoted only when ambiguous.

## Go Coding Standards

The Go codebase follows the **`samber/cc-skills-golang`** community skill pack
(MIT-licensed) — a set of 12 skills that cover concurrency, context, data
structures, design patterns, error handling, gRPC, modernization, safety,
security, structs/interfaces, testing, troubleshooting. Each skill is loaded
locally at `~/.agents/skills/golang-*/SKILL.md`.

The full distilled standard for this project lives in
[`docs/architecture/07-go-coding-standards.md`](docs/architecture/07-go-coding-standards.md)
(coding standards) and
[`docs/architecture/08-tdd-discipline.md`](docs/architecture/08-tdd-discipline.md)
(test-driven development discipline + gin testing patterns).
Headlines (enforced mechanically by `.golangci.yml` where possible):

1. **Errors** — `fmt.Errorf("ctx: %w", err)` for wrapping; `errors.Is/As` for inspection;
   single handling rule (log OR return, never both); low-cardinality strings — variable
   data goes to `slog.Attr`/`oops.With`, not into the error message
   (`samber/cc-skills-golang@golang-error-handling`).
2. **Context** — `ctx context.Context` first parameter, propagated through the entire
   call chain; never stored in a struct; `defer cancel()` immediately after
   `WithCancel/WithTimeout/WithDeadline`; `context.WithoutCancel` for background work
   that must outlive the parent request (`samber/cc-skills-golang@golang-context`).
3. **Concurrency** — every goroutine has a clear exit (ctx.Done/done channel/WaitGroup);
   `errgroup.WithContext` + `SetLimit(n)` for worker pools; `time.NewTimer + Reset` over
   `time.After` in loops; `goleak.VerifyTestMain` in every package that spawns goroutines
   (`samber/cc-skills-golang@golang-concurrency`).
4. **Interfaces** — small (1–3 methods); defined where consumed, not where implemented;
   accept interfaces, return concrete structs; `var _ api.X = (*concrete.X)(nil)`
   compile-time check; no premature interfaces for single implementation
   (`samber/cc-skills-golang@golang-structs-interfaces`).
5. **Safety** — comma-ok type assertions only (`forcetypeassert` linter); typed nil ≠ nil
   in interfaces; never write to nil map; `defer` extracted from loops; integer
   conversions bounds-checked; epsilon for float comparison
   (`samber/cc-skills-golang@golang-safety`).
6. **Security** — `crypto/rand` for tokens/keys, `math/rand/v2` for non-security only;
   AES-GCM, never CBC/ECB; parameterized SQL; `crypto/subtle.ConstantTimeCompare` for
   secret comparison; `govulncheck` in CI; `gosec` linter (`samber/cc-skills-golang@golang-security`).
7. **Modernize** — Go 1.22+ idioms: `any` over `interface{}`, `min/max/clear` builtins,
   `range` over int, `slices`/`maps` packages, `t.Context()` in tests, `cmp.Or` for
   defaults (`samber/cc-skills-golang@golang-modernize`).
8. **Testing** — table-driven with named subtests; `t.Parallel()` (paralleltest linter);
   `//go:build integration` for testcontainers-go tests; testify as helpers, not
   replacement; `goleak` for goroutine leaks; race detector required in CI
   (`samber/cc-skills-golang@golang-testing`).
9. **HTTP — gin** (ADR-0014). Handlers are `func(c *gin.Context)`. JSON binding
   via `c.ShouldBindJSON(&dto)`, JSON output via `c.JSON(status, resp)`. URL
   params via `c.Param("id")`, query via `c.Query("q")`. Tests use
   `gin.SetMode(gin.TestMode)` + `httptest.NewRecorder()`; see
   [`docs/architecture/08-tdd-discipline.md`](docs/architecture/08-tdd-discipline.md)
   § "Gin-specific HTTP Test Pattern".

When a PR violates one of these, link the offending `samber/cc-skills-golang@<skill>`
section in the review comment. The skill pack itself is the source of truth for
rationale and examples.

## Module boundaries

Cross-module Go imports allowed **only** between `internal/<module>/api` packages.
Importing another module's `service`, `store`, or `events` is a lint error.
See [internal/README.md](internal/README.md).

## Testing

- **Unit tests:** Go `testify` (helpers, not replacement), TS `vitest`. Coverage ≥ 70%
  on business logic, ≥ 90% for `dialer.FSM`, `surveys.Runtime`, `RDDGenerator`, `Router`.
  Table-driven with named subtests + `t.Parallel()` enforced by `paralleltest`. See
  `samber/cc-skills-golang@golang-testing`.
- **Integration tests:** `testcontainers-go` against ephemeral Postgres/Redis/NATS,
  `//go:build integration` tag, run via `go test -tags=integration ./...`.
- **Goroutine leak detection:** `go.uber.org/goleak` `VerifyTestMain` in every package
  that spawns goroutines (`pkg/outbox`, `internal/dialer/service`,
  `internal/realtime/service`, `internal/telephony/...`).
- **E2E tests:** Playwright in `tests/e2e/`.
- **Race detector** required: `make test` runs `go test -race -count=1 ./...`.

## Secrets

Never commit:
- `.env` files (only `.env.example` is allowed)
- `*.pem`, `*.key`, `*.crt`
- Hardcoded passwords/tokens

CI runs `gitleaks` to catch leaked secrets.

## Design changes

Significant architectural changes require an ADR. Add it to the system design
doc (§22) or as a standalone file under `docs/adr/NNNN-<topic>.md` (4-digit
zero-padded numbering).

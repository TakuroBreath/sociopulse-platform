# 07. Go Coding Standards

This document distils the **`samber/cc-skills-golang`** community skill
pack (MIT, 12 skills under `~/.agents/skills/golang-*/`) into the
project-specific Go coding standard. It is the authoritative version
that `CONTRIBUTING.md` § "Go Coding Standards" links to. When a rule
here disagrees with a skill file's text, the skill file wins for
rationale and deeper examples; this document wins for project-specific
adaptations (gin, zap, mockery, the `internal/<module>/api/` boundary).

The full Red-Green-Refactor playbook lives in `08-tdd-discipline.md`.
Module-by-module API contracts live in `02-module-contracts.md`. The
linter that mechanically enforces this document is `.golangci.yml`.

## 1. Skill Pack Inventory

The skill pack ships twelve skills, each loaded locally at
`~/.agents/skills/golang-<area>/SKILL.md`. They auto-trigger by
description match when working with relevant code; four of them are also
user-invocable as slash commands (`/golang-modernize`,
`/golang-security`, `/golang-testing`, `/golang-troubleshooting`).

| # | Skill | One-line summary |
|---|---|---|
| 1 | `golang-error-handling` | `%w` wrapping, `errors.Is/As`, sentinel design, `samber/oops` at the boundary, structured logs |
| 2 | `golang-context` | First-parameter `ctx`, propagation, `WithoutCancel`, `WithTimeout/Deadline`, immediate `defer cancel()` |
| 3 | `golang-concurrency` | `errgroup.WithContext` + `SetLimit`, channel ownership, `time.NewTimer + Reset` over `time.After`, `goleak` |
| 4 | `golang-data-structures` | Slice growth and preallocation, `slices`/`maps` packages, `strings.Builder`, generic constraints |
| 5 | `golang-design-patterns` | Functional options, lifecycle and resource-management, graceful shutdown, DI via constructors |
| 6 | `golang-error-handling` (cont.) | low-cardinality strings, single-handling rule, `errors.Join`, `slog.Attr` / `oops.With` |
| 7 | `golang-grpc` | health-check service, `GracefulStop`, `status.Errorf` with `errdetails`, mTLS, bufconn tests |
| 8 | `golang-modernize` | Go 1.22+ idioms (`any`, `min/max/clear`, range over int, `slices`/`maps`), Go 1.24 `t.Context()`, Go 1.25 `wg.Go()` |
| 9 | `golang-safety` | comma-ok type assertions, typed nil ≠ nil interface, no nil-map writes, no `defer` in loops, bounds-checked numeric conversion |
| 10 | `golang-security` | `crypto/rand` for tokens/keys, AES-GCM only, parameterised SQL, `crypto/subtle.ConstantTimeCompare`, `gosec`, `govulncheck` |
| 11 | `golang-structs-interfaces` | small interfaces (1-3 methods), defined where consumed, accept-interfaces / return-structs, compile-time `var _ I = (*T)(nil)` checks |
| 12 | `golang-testing` | table-driven + `t.Parallel()`, `//go:build integration`, testify as helper, mockery, goleak |
| 13 | `golang-troubleshooting` | reproduce-before-fix, race detector, goleak diagnostics, `pprof` on admin port, Delve in dev only |

(`golang-error-handling` is split across rows 1 and 6 above to make the
themes legible — there is one skill file, the standard reflects two
distinct sets of rules drawn from it.)

## 2. Errors

Errors live in three layers:

1. **Sentinel errors** in `internal/<module>/api/errors.go`:
   `var ErrXxx = errors.New("module: short description")`.
2. **Wrapping** through the call chain with
   `fmt.Errorf("verb noun: %w", err)`. Always `%w`, never `%v` for an
   `error` argument.
3. **Inspection** at the boundary with `errors.Is` for sentinels and
   `errors.As` for typed errors.

Why low-cardinality strings matter: log aggregators index by message.
A message string that interpolates `tenant_id`, `respondent_id`, or
`call_id` blows up the index and makes "show me every `auth: token
invalid` over the last hour" return zero matches. Variable data goes
into structured fields:

```go
return fmt.Errorf("get project by code %q: %w", code, api.ErrProjectNotFound)
//                                  └── ok, low-cardinality (project codes: ~hundreds per tenant)
//                                      keep variable data in fields:
log.Error("project lookup failed", zap.String("code", code), zap.Error(err))
```

The single-handling rule: **log OR return, never both**. The function
that creates the error returns it wrapped; the outermost handler
(HTTP, gRPC, NATS, asynq) logs it once. `03-error-handling.md` has the
full mapping table from sentinels to gin error envelopes and gRPC
status codes.

`samber/oops` is reserved for the outermost handler layer
(`pkg/httputil/error_handler.go`,
`pkg/grpc/middleware/error.go`, `internal/<module>/events/subscriber.go`,
`cmd/worker/...`). At the boundary, attach structured context with
`oops.With(...)` so it lands in the zap logger automatically. Inside
services and stores, plain `fmt.Errorf` is enough.

`errors.Join` is the canonical way to aggregate multiple failures
(parallel fan-out, multi-phase save). Wrap the joined value with the
caller's intent:

```go
errs := errors.Join(saveProject(ctx), saveQuotas(ctx), saveAssignments(ctx))
return fmt.Errorf("save project tree: %w", errs)
```

## 3. Context

Every function that does I/O, blocks, or might be cancelled accepts
`ctx context.Context` as its **first** parameter. The same `ctx`
propagates through HTTP handler → service → store → external client
without forking. Three rules:

1. **Always first parameter.** `func (s *Service) Foo(ctx context.Context, req X) error`.
   The convention is project-wide; the `contextcheck` linter enforces
   propagation through call chains.
2. **`defer cancel()` immediately** after `WithTimeout`, `WithDeadline`,
   `WithCancel`. Forgetting `cancel()` leaks goroutines and timers.
3. **`context.WithoutCancel`** for background work that must outlive
   the parent request. Examples in this codebase: outbox relay
   publishing to NATS after the originating HTTP request closed; audit
   log write that we deliberately want to complete even if the user
   navigated away.

Never store `ctx` in a struct. The `revive` rule + reviewer scrutiny
catch this. Pass it through arguments. The exception is internal
goroutines whose lifecycle is owned by the constructor (a `Run(ctx)`
method that holds `ctx` for the duration of the run is fine — but the
caller is the one who cancels it).

The `noctx` linter catches `http.NewRequest(...)` (no context); use
`http.NewRequestWithContext`. Same for `(*sql.DB).Query` — use
`QueryContext`. There is no good reason for context-less I/O in our
codebase.

## 4. Concurrency

Every goroutine has a clear exit. The default skeleton:

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(workerN)
for i := 0; i < jobN; i++ {
    job := jobs[i]
    g.Go(func() error {
        return process(ctx, job)
    })
}
if err := g.Wait(); err != nil { return err }
```

Three things to notice: `errgroup.WithContext` cancels siblings on the
first error; `SetLimit(n)` caps in-flight goroutines; `g.Wait()` is
the single-rendezvous point that returns the first error.

Channel ownership: **only the sender closes**. Always declare channel
direction at the boundary:

```go
func produce(ctx context.Context, out chan<- Item) error { ... }
func consume(ctx context.Context, in  <-chan Item) error { ... }
```

A receiver closing a channel is a bug; sending on a closed channel
panics. Both are caught by tests run under `-race` against
representative workloads.

`time.After` in a loop **is forbidden**. It allocates a fresh timer on
every iteration that the runtime cannot reclaim until the duration
expires. Use `time.NewTimer + Reset`:

```go
t := time.NewTimer(0)
defer t.Stop()
for {
    if !t.Stop() { <-t.C }
    t.Reset(delay)
    select {
    case <-ctx.Done(): return ctx.Err()
    case <-t.C: /* tick */
    }
}
```

`make grep-time-after` (Plan 00a Task 8) is a CI guard.

`go.uber.org/goleak.VerifyTestMain` is **mandatory** in every package
that spawns a goroutine in production code. Our list:
`pkg/outbox`, `internal/dialer/service`, `internal/realtime/service`,
`internal/telephony/...`, `internal/recording/service`,
`internal/analytics/service`, `internal/reports/service`. Race detector
required: every CI run uses `go test -race -count=1 ./...`. A race
finding is never a flaky test — it is a bug.

## 5. Interfaces and Structs

Interfaces are small (1-3 methods), composed when larger contracts are
needed, and **defined where consumed**:

```go
// internal/auth/service/authenticator.go — defines what THIS service needs
type RefreshTokenStore interface {
    Save(ctx context.Context, jti string, rec RefreshRecord) error
    Lookup(ctx context.Context, jti string) (RefreshRecord, error)
    Rotate(ctx context.Context, oldJTI, newJTI string, rec RefreshRecord) (bool, error)
    Delete(ctx context.Context, jti string) error
}
```

The Redis adapter in `internal/auth/store/refresh_store.go` does NOT
declare it implements `RefreshTokenStore`. The compile-time check
proves that anyway:

```go
var _ service.RefreshTokenStore = (*store.RefreshStore)(nil)
```

Place this line near the adapter type. It catches drift the moment
the interface adds or removes a method.

The exception to "defined where consumed" is the `internal/<module>/api/`
package — those interfaces are intentional cross-module seams. They
are the project's contract surface and `02-module-contracts.md`
specifies them precisely. New `api/` interfaces require an architecture
review (a small note in the relevant plan, not a separate ADR).

Constructors return concrete types, not interfaces:

```go
func NewAuthenticator(deps Deps) *Authenticator { ... }
```

Why: returning an interface from a constructor makes test assertions
on the concrete type awkward, and adds an implicit indirection that
`mockery` doesn't help with.

Pointer vs value receivers: pointer when the type has any state
(mutex, atomic counter, pool, slice/map fields), value for tiny
immutable DTOs. Mixing the two on the same type is a `revive` warning.

## 6. Safety

Defensive coding to prevent panics, silent corruption, and subtle
runtime bugs:

- **Comma-ok type assertion only.** `forcetypeassert` linter blocks
  bare assertions:

  ```go
  // forbidden
  s := v.(string)

  // required
  s, ok := v.(string)
  if !ok { return fmt.Errorf("expected string, got %T", v) }
  ```

- **Typed nil ≠ nil interface.** A common `(*T)(nil)` returned through
  an `error` slot becomes a non-nil interface that fails the
  `if err == nil` check. The `nilerr` linter catches the most common
  pattern (`return err` where err is `*MyErr` and could be a typed
  nil). Reviewer scrutiny catches the rest.

- **Never write to a nil map.** Always initialise:

  ```go
  m := map[string]int{}
  // not: var m map[string]int
  ```

  Reads from a nil map return the zero value (safe); writes panic.

- **No `defer` in tight loops.** Each `defer` allocates a node in the
  defer stack. The pattern that's flagged in review:

  ```go
  for _, file := range files {
      f, err := os.Open(file)
      if err != nil { return err }
      defer f.Close()  // ← accumulates until function exit
      // ...
  }
  ```

  Refactor: extract a closure or call `f.Close()` inline.

- **Bounds-checked numeric conversion.** Going `int → int32` may
  truncate on a 64-bit host:

  ```go
  // forbidden — silent overflow risk
  return int32(n)

  // required
  if n > math.MaxInt32 || n < math.MinInt32 {
      return 0, fmt.Errorf("value %d overflows int32", n)
  }
  return int32(n), nil
  ```

  `gosec:G115` flags these in audited paths.

- **Float comparison via epsilon.** `==` on floats is almost always
  wrong. Use a small tolerance:

  ```go
  const epsilon = 1e-9
  if math.Abs(a-b) < epsilon { /* equal */ }
  ```

The `samber/cc-skills-golang@golang-safety` skill file enumerates each
rule with examples; the linter rules in `.golangci.yml`
(`forcetypeassert`, `nilerr`, `gosec`) catch the mechanical violations.

## 7. Security

Cryptography and security primitives have no room for "close enough":

- **`crypto/rand` for tokens, session IDs, API keys, recording DEKs.**
  The `golang-security` skill is unambiguous here. `math/rand/v2` is
  fine for non-security randomness (jitter, sampling, test data).
  `math/rand` v1 is forbidden by the depguard rule
  `banned-stdlib`:

  ```yaml
  - pkg: "math/rand"
    desc: "use math/rand/v2 for non-security randomness or crypto/rand for tokens/keys"
  ```

- **AES-256-GCM only.** CBC, ECB, DES, MD5, SHA1 are all banned by
  depguard:

  ```yaml
  - pkg: "crypto/cipher.NewCBCEncrypter"
    desc: "CBC lacks authentication — use AES-GCM"
  - pkg: "crypto/des"
    desc: "DES is broken — use AES-256"
  - pkg: "crypto/md5"
    desc: "MD5 is broken — use SHA-256+ or argon2id"
  - pkg: "crypto/sha1"
    desc: "SHA-1 has known collision attacks — use SHA-256+"
  ```

  The encryption pathway: per-tenant KEK in Yandex KMS,
  `KMS.GenerateDataKey` produces a 32-byte DEK + KMS-wrapped
  ciphertext, AES-256-GCM encrypts the payload with the DEK and a
  random nonce, both ciphertext and wrapped-DEK are stored together.
  The `pkg/encryption` package is the single implementation point.

- **Parameterised SQL only.** No string concatenation into queries.
  pgx v5 parameter binding is the project default; `sqlc` generates
  parameter-safe wrappers. `gosec:G201/G202` catch the worst
  offenders.

- **`crypto/subtle.ConstantTimeCompare` for secrets.** Comparing TOTP
  codes, refresh-token hashes, mTLS SAN strings — anything an attacker
  could probe for timing differences. `==` on a `[]byte` is a timing
  oracle.

- **`gosec` linter + `govulncheck` in CI.** `gosec` runs on every PR.
  `govulncheck` runs nightly on `main` and on PRs that change
  `go.mod`. A new vuln finding fails the build.

## 8. Modernize

We are on Go 1.26.1 and use it like it. Common modernisations:

- `any` over `interface{}` for empty interfaces.
- `min` / `max` / `clear` builtins (Go 1.21).
- `range` over `int` (Go 1.22): `for i := range 10`.
- `slices` / `maps` standard packages — `slices.Sort`,
  `slices.Contains`, `maps.Clone`, etc. Replace hand-rolled equivalents.
- `cmp.Or` for fallback chains: `cmp.Or(s.A, s.B, "default")`.
- `sync.OnceValue` / `sync.OnceFunc` over hand-rolled sync.Once
  patterns.
- `errors.Join` for aggregating multiple errors.
- `slog.Attr` builders when slog appears (we are still on zap; this
  applies after the ADR-0016 migration).
- `t.Context()` (Go 1.24+) in tests instead of `context.Background()` —
  cancels on test failure / cleanup.
- `wg.Go()` (Go 1.25): wraps the typical
  `wg.Add(1); go func() { defer wg.Done(); ... }()` boilerplate.

The `samber/cc-skills-golang@golang-modernize` skill auto-triggers when
it sees an old-style pattern; in PR review, prefer the modern form.

## 9. Testing

Testing is covered in depth in `04-testing-strategy.md` and
`08-tdd-discipline.md`. The core rules:

- **Table-driven + named subtests** is the default shape. `t.Run("zero
  quantity returns zero", ...)`.
- **`t.Parallel()` mandatory** on every test and every subtest.
  `paralleltest` enforces.
- **`t.Helper()` in helpers** so `t.Errorf` reports the caller's line.
  `thelper` enforces.
- **`testify` as helper, not replacement.** `require.X` for setup,
  `assert.X` for individual properties.
- **`testify/suite` is forbidden.** Hides the boundary, breaks
  `t.Parallel()`, conflicts with `goleak`.
- **`testify/mock` is forbidden.** We use `mockery v2` to generate one
  mock per `api/` interface.
- **Build tag `//go:build integration`** for testcontainers tests.
  Run separately with `go test -tags=integration -race -count=1 ./...`.
- **`goleak.VerifyTestMain`** in every package with goroutines.
- **TDD mandatory** per ADR-0015. See `08-tdd-discipline.md`.

Coverage targets are in `04-testing-strategy.md` § Coverage Targets.

## 10. gRPC

gRPC is used in two places only:

- **`internal/recording/grpc/`** — the gRPC server in `cmd/api`
  consumed by `cmd/recording-uploader`. Methods: `Commit`, `Get`,
  `GetPresignedURL`. mTLS with cert pinning per FS-VM identity.
- **`internal/telephony/grpc/`** — the control-plane gRPC between
  `cmd/api` and `cmd/telephony-bridge` for the few flows that don't
  fit NATS (e.g. live `Stats()` aggregation across replicas). mTLS
  via service-account certificates.

Both use:

- `google.golang.org/grpc` standard library.
- **Health check service** (`grpc.health.v1`) registered on every
  server.
- **`server.GracefulStop()`** with a 30-second timeout in shutdown.
- **`status.Errorf(codes.X, ...)`** with `errdetails` for structured
  problems. Mapping from sentinel errors lives in
  `pkg/grpc/errors.go` (single source).
- **mTLS** via `pkg/grpc.NewMTLSServer` and `pkg/grpc.NewMTLSClient`.
  The CA bundle and server cert paths come from config.
- **Reflection disabled in production** (`reflection.Register`
  conditional on `service.env == "development"`). This is a security
  hardening step — we do not want production services advertising
  their schema to anyone who can dial.

Tests use `bufconn`:

```go
lis := bufconn.Listen(1 << 20)
s := grpc.NewServer()
pb.RegisterRecordingServiceServer(s, recordingSvc)
go s.Serve(lis)
defer s.Stop()

conn, err := grpc.NewClient("passthrough://bufnet",
    grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
    grpc.WithTransportCredentials(insecure.NewCredentials()))
```

## 11. Troubleshooting

Bugs follow a fixed playbook from `golang-troubleshooting`:

1. **Reproduce first.** Write a failing test that captures the bug. If
   you cannot reproduce, you cannot fix.
2. **Read the error message fully.** Stack trace, line, variable —
   read it before forming a hypothesis.
3. **One hypothesis at a time.** Change one variable; re-run; observe.
4. **Fix the root cause.** A "symptom fix" (a nil check that papers
   over the bug) is forbidden. The PR description must include
   `Root cause: ...`.

Diagnostic tools:

- **Race detector** + **goleak** are usually the first answer for
  concurrency bugs.
- **`pprof` on the admin port** (`cmd/api` exposes `/debug/pprof` on
  a separate listener with auth required, never on the public port).
  CPU / memory / goroutine profiles via `go tool pprof`.
- **Delve in dev only.** Production processes never run under a
  debugger. The `pprof` profiles plus structured logs cover production
  diagnostics.

The new test stays in the suite as a regression guard.

## 12. Linter Mapping

Mechanical enforcement of every rule above. The full list lives in
`.golangci.yml` (35 linters); below is the mapping from this document's
rules to the specific linter that enforces each:

| Rule | Linter |
|---|---|
| `%w` for error wrapping | `errorlint:errorf` |
| `errors.Is/As` over `==`/type assertion | `errorlint:comparison`, `errorlint:asserts` |
| `ErrXxx` naming for sentinels | `errname` |
| Comma-ok type assertion | `forcetypeassert` |
| HTTP request without context | `noctx` |
| Context propagation through chain | `contextcheck` |
| `t.Parallel()` in tests | `paralleltest` |
| `t.Helper()` in helpers | `thelper` |
| testify idioms (`require` vs `assert`, `Equal` arg order, ...) | `testifylint` |
| slog/zap key-value pairs correct | `loggercheck` |
| Exhaustive switch over enum | `exhaustive` |
| Module isolation `internal/X/api/` only | `depguard:module-boundaries` |
| `math/rand`, weak crypto banned | `depguard:banned-stdlib` |
| Competing routers (chi/echo/fiber) banned | `depguard:banned-third-party` |
| Body close on `*http.Response` | `bodyclose` |
| `rows.Close()` and `rows.Err()` | `sqlclosecheck`, `rowserrcheck` |
| Security patterns (gosec rules) | `gosec` |
| Vulnerability scan | `govulncheck` (CI job, nightly + on `go.mod` change) |
| Dead code | `unused` |
| Ineffectual assignments | `ineffassign` |
| Inefficient string concatenation | `prealloc` |
| Unused return values | `unparam` |
| Naked returns | `nakedret` |
| `nil != nil` interface confusion | `nilerr` |
| Wasted assignments | `wastedassign` |
| Unconvert | `unconvert` |
| Typecheck (compile-time) | `typecheck`, `govet` |
| Static analysis | `staticcheck` |
| Cyclomatic complexity | `gocyclo` |
| Cognitive complexity | `gocognit` |
| Misspellings | `misspell` |
| Whitespace | `whitespace` |
| Format / imports | `gofmt`, `goimports` |
| Style and idiom (var-naming, package-comments, error-naming, ...) | `revive` |

Configuration knobs that matter:

- `goimports.local-prefixes: github.com/sociopulse/platform` — local
  imports sort into their own block.
- `errorlint.errorf: true` — require `%w` for error wrapping.
- `errorlint.errorf-multi: true` — also enforce on multi-arg
  `Errorf`.
- `errorlint.asserts: true` — require `errors.As` over type assertion
  on errors.
- `errorlint.comparison: true` — require `errors.Is` over `==`.
- `paralleltest.ignore-missing: false` — every test SHOULD call
  `t.Parallel()` unless explicitly excluded.
- `loggercheck.zap: true`, `loggercheck.slog: true`,
  `loggercheck.require-string-key: true` — both modes; flip after
  ADR-0016 migration.
- `exhaustive.default-signifies-exhaustive: true` — a `default:` in a
  switch counts as exhaustive (Go idiom).
- `gocyclo.min-complexity: 15`, `gocognit.min-complexity: 20` —
  enforced everywhere except `cmd/.*/main.go`.

Test files have a small set of relaxations
(`issues.exclude-rules.path: _test\.go`):

- `gosec` — tests use predictable seeds, hard-coded credentials in
  fixtures.
- `gocognit`, `gocyclo` — table-driven tests are intentionally branchy.
- `errcheck` — tests deliberately ignore some return values.

Three rules from this document are enforced **only by review**, not by
linter:

- The single-handling rule (log OR return).
- Premature interfaces in `service/` (extract on second consumer).
- Low-cardinality error message strings.

Reviewers cite the relevant `samber/cc-skills-golang@<skill>` section
when a PR violates one of these — the skill file is the rationale, this
document is the ruling.

## Cross-references

- `08-tdd-discipline.md` — Red-Green-Refactor playbook.
- `04-testing-strategy.md` — pyramid, coverage, mockery / goleak setup.
- `03-error-handling.md` — sentinel → HTTP / gRPC mapping.
- `05-configuration.md` — runtime config layering.
- `06-observability.md` — logging fields, metric names, span names.
- `02-module-contracts.md` — every public interface in `api/`.
- `01-package-layout.md` — directory rules, naming.
- `00-overview.md` — module map and dependency graph.
- `samber/cc-skills-golang` — skill files at `~/.agents/skills/golang-*/SKILL.md`.
- ADR-0012 — zap.
- ADR-0014 — gin.
- ADR-0015 — TDD as mandatory discipline.

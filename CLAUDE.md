# CLAUDE.md

Guide for Claude when working in this repository.

## Project context

This is one of three repositories in the **СоциоПульс** project — a multi-tenant SaaS for telephone sociological surveys (call-centres running political/social polls). Target scale: 30 tenants, 500 concurrent operators, 50k calls/day.

| Repo | Role | URL |
|---|---|---|
| `sociopulse-platform` | Backend Go monorepo (cmd/api + sidecars + workers) | https://github.com/TakuroBreath/sociopulse-platform |
| `sociopulse-web` | React + TypeScript frontend | https://github.com/TakuroBreath/sociopulse-web |
| `sociopulse-infra` | Terraform / Packer / Ansible / Helm IaC | https://github.com/TakuroBreath/sociopulse-infra |

This is the **`sociopulse-platform`** repo — backend Go monorepo.

Implementation specification, architecture decisions, and 22 implementation plans live in [`docs/superpowers/`](docs/superpowers/) — this repo is the **master location** for all project documentation. Frontend (`sociopulse-web`) and infra (`sociopulse-infra`) repos reference these documents via GitHub URLs. Plans relevant to this repo: 00 (foundation), 00a (architecture), 02-14, 20 Task 1.

## Identity

Local git config in this repo: `TakuroBreath / maxsmurffy@gmail.com` (set via `git config --local`). The global git config on this machine is a different user — never rely on it for commits here.

`gh` CLI is authenticated as `TakuroBreath`.

## Agent skills

### Issue tracker

Issues live in GitHub Issues for this repo (`TakuroBreath/sociopulse-platform`). See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical labels (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`) — defaults, already created on GitHub. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout — one `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.

### Go coding skills (samber/cc-skills-golang)

The Go codebase follows the `samber/cc-skills-golang` community skill pack
(MIT, 12 skills) installed locally at `~/.agents/skills/golang-*/SKILL.md`.
Skills auto-trigger by description match; four are user-invocable:
`/golang-modernize`, `/golang-security`, `/golang-testing`, `/golang-troubleshooting`.

Distilled project-specific standards: [`docs/architecture/07-go-coding-standards.md`](docs/architecture/07-go-coding-standards.md)
(created in Plan 00a Task 1). Mechanical enforcement via `.golangci.yml`
+ depguard (Plan 00 Task 9, Plan 00a Task 8). Spec reference: §17.8 in
[`docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`](docs/superpowers/specs/2026-05-06-sociopulse-system-design.md).

Headlines:
1. Errors: `fmt.Errorf("ctx: %w", err)`, single-handling rule, low-cardinality strings.
2. Context: `ctx context.Context` first param, `context.WithoutCancel` for outlive-parent work.
3. Concurrency: clear goroutine exit, `errgroup.SetLimit` over hand-rolled pools, `goleak.VerifyTestMain`.
4. Interfaces: small, defined where consumed; accept interface, return struct; `var _ api.X = (*Y)(nil)` compile-check.
5. Safety: comma-ok type assertion, no `defer` in loops, bounds-checked numeric conversion.
6. Security: `crypto/rand` for tokens, AES-GCM only, parameterized SQL, `crypto/subtle.ConstantTimeCompare`.
7. Testing: table-driven + `t.Parallel()`, `//go:build integration`, `goleak`.

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

## Workflow rule for this project

When working on Plans (00a/00b/02/03/...) on this repo:

1. **Compact context** at the start — read tags, last commits, current state of `internal/`, `pkg/`, `migrations/`, `cmd/`. Cross-reference with `PROJECT_STATUS.md` (the living state document; update it after every milestone).
2. **Read the plan** — `docs/superpowers/plans/2026-05-06-NN-<topic>.md`. Extract every task. Create a TodoWrite list.
2a. **Read the references** — `docs/references/plan-NN-<topic>.md` (per-plan curated reading list) + `docs/references/COMMON.md` (cross-cutting). If the per-plan file doesn't exist yet, **create it before dispatching the first subagent** — it captures canonical specs, reference impls, gotchas, open questions. Subagent prompts MUST include the file path so they read it before writing code.
3. **Per task**, dispatch a fresh implementer subagent (`Agent` tool, `general-purpose`, `model: opus`) with:
   - Explicit reference to relevant `samber/cc-skills-golang` skills (e.g., `golang-concurrency` BP1-BP9 for goroutine work, `golang-security` for crypto, `golang-error-handling` for error policy).
   - **TDD discipline mandatory** per `superpowers:test-driven-development`: Red-Green-Refactor, watch the test fail before writing impl.
   - Path-correction note: many older plan texts use `internal/<X>` where the real path is `pkg/<X>` (Plan 00a moved several abstractions to `pkg/`). Always check existing scaffolding before instructing the agent.
   - Quality bar: `go build ./...` + `go vet ./...` + `go test -race -count=1 ./...` + `golangci-lint run ./...` + `gofmt -l ...` + `make grep-time-after`. ALL must be green before reporting DONE.
   - Subagent MUST commit at the end (don't leave uncommitted work to be found later).
4. **Two-stage review** per task (per `superpowers:subagent-driven-development`):
   - Spec compliance review (verify code matches plan).
   - Code quality review (strengths/issues/severity).
   - Fix-up loop until both pass.
5. **gopls cache is often stale** after subagent dispatches — diagnostic noise. Always re-verify directly with `go build ./... && go vet ./... && go test -race -count=1 ./...`. If those pass, the IDE diagnostics are noise.
6. **At the end of each plan**, push to origin/main, watch CI to green (6 jobs: lint/test/build/docker/vuln/secret-scan), then tag `v0.0.N-<plan-slug>`.
7. **Update `PROJECT_STATUS.md`** after each plan completes. Future agents read this first to know what exists. This is mandatory — `superpowers:grill-with-docs` is the canonical way (skill name from user; if not available in the local skill cache, do the equivalent manually: audit git log + actual changes, update PROJECT_STATUS.md to match).
8. **Update `docs/references/plan-NN-<topic>.md`** after each plan completes — fill the "Production lessons" section with what we actually learned, especially gotchas not in the canonical specs. Future agents (and you) re-reading this file save real time.

## Tooling notes

- `/Users/user/go/bin/golangci-lint` is the lint binary (v1.64.8 built against Go 1.26).
- `make grep-time-after` is the CI gate against `time.After` in for loops (samber/cc-skills-golang@golang-concurrency § BP6).
- `make dev-up` boots Postgres + Redis + NATS in containers; cmd/api etc. run natively via `go run`.
- testcontainers-go is wired in `pkg/postgres`, `pkg/outbox`, `cmd/migrator` — local Docker required for `-tags=integration` tests.
- depguard rules in `.golangci.yml`:
  - `module-boundaries`: `internal/<X>/{service,store,events}` not importable across modules.
  - `pgxpool-isolation`: `pgxpool` only in `pkg/postgres` + `internal/tenancy/store/admin_*` + `cmd/migrator`.
  - `yandex-sdk-isolation`: forward-looking guard for Plan 04 KMS work.
  - `banned-stdlib`: `math/rand`, CBC/ECB, MD5, SHA1, DES.

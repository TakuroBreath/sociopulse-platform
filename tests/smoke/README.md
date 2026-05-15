# tests/smoke/

End-to-end "boot the whole `cmd/api` against a real Postgres+Redis+NATS testcontainer stack" tests.

## Run

    make test-smoke
    # OR
    go test -tags=smoke -race -count=1 -timeout=15m ./tests/smoke/... ./cmd/api/...

Requires Docker. Cold runs ~90 s (image pulls); cached runs ~30 s.

## Architecture

- **Per-`TestMain` shared stack.** One set of Postgres + Redis + NATS containers, used by every smoke test in the binary. Per-test isolation is via `stack.Reset(t)` (truncates per-tenant rows) when scenarios need a clean slate; the default is per-test additive seeding.
- **`cmd/api` runs as a goroutine, not a binary.** Mirrors `cmd/api/main_test.go::TestRunStartsAndShutsDownCleanly` — same composition-root seam. No `os/exec`.
- **Build tag `//go:build smoke`.** Untagged builds (`go build ./...`, `make ci`) ignore the smoke package entirely.
- **Migrations applied inline.** Uses `golang-migrate/migrate/v4` directly against the testcontainer DSN — no `cmd/migrator` refactor required.

## Where the test code lives

Smoke tests live under `cmd/api/` (e.g. `cmd/api/smoke_test.go`) so they can call the unexported `main.run()` composition root. The reusable testcontainer-stack lifecycle + config writer + HTTP helpers live in this package (`tests/smoke/`) as a library that `cmd/api/smoke_test.go` imports.

Why split this way:

- `package main` cannot be imported from outside `cmd/api`, so smoke tests that drive `run()` must reside inside `cmd/api/`.
- The harness library — testcontainer wiring, config writer, HTTP helpers — is reusable across smoke scenarios and lives in `tests/smoke/` per the plan structure.

## CI

The `smoke` job in `.github/workflows/ci.yml` (added in Plan 21 Task 8) runs on every push to `main` and on every `v*` tag push. Tag-push deploys gate on smoke green.

## Gotchas

- `TESTCONTAINERS_RYUK_DISABLED=true` is recommended on macOS where the ryuk reaper container has had Docker-version compatibility trouble. Trade-off: a panicking test may leak containers.
- testcontainers-go API has churned through 2025; if dial errors surface, run `make test-smoke` from a freshly-pulled `postgres:16-alpine`, `redis:7-alpine`, `nats:2.10-alpine`.
- The smoke config sets `analytics.enabled: false` — Phase 1 does not exercise ClickHouse. Plan 21 Tasks 5-7 add scenarios that stay within the Postgres+Redis+NATS surface.

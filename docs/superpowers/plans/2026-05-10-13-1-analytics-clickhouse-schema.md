# Plan 13.1 — ClickHouse Schema Foundation (analytics)

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development`
> to execute task-by-task with TDD + 2-stage review. Steps use checkbox (`- [ ]`)
> syntax for tracking.
>
> **Plan ID:** 13.1
> **Sub-plan of:** Plan 13 — Analytics + Reports (`docs/superpowers/plans/2026-05-06-13-analytics-reports.md`)
> **References:** `docs/references/plan-13-analytics.md` (read FIRST)
> **Master spec:** `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` §6.4, §FR-I, §17
> **ADRs:** `docs/adr/0010-postgres-plus-clickhouse.md` (governs the choice)
>
> **Out of scope (deferred):**
> - Plan 13.2 — IngestPipeline (NATS → CH) + MetricsQuery + Redis cache + HTTP
> - Plan 13.3 — Reports module (XLSX/CSV/PDF + asynq + S3)

**Goal:** Land the ClickHouse schema foundation: 3 source event tables
(`events_calls`, `events_operator_state`, `events_recording_uploaded`) +
3 materialised views (`mv_calls_hourly`, `mv_operator_kpi_daily`,
`mv_quotas_progress`), and extend `cmd/migrator` to apply the CH
migration set from a dedicated `migrations/clickhouse/` directory.

**Architecture:** Reuse the existing `cmd/migrator` binary by adding a
`--target=clickhouse` flag (defaulting to `postgres` for backward
compat). CH-mode reads `CLICKHOUSE_DSN` + `CLICKHOUSE_MIGRATIONS_PATH`
env vars. Schema-shape tests live in `cmd/migrator/integration_ch_test.go`
(parallel to the existing `integration_test.go`), boot a
`testcontainers-go/modules/clickhouse` container, apply migrations,
introspect via `system.columns` / `system.tables`, and assert column
types + engine + partition/order keys. Materialised view rollup tests
insert raw fixtures, force `OPTIMIZE … FINAL`, query via `*Merge`
finals, and assert the rollup matches the hand-computed expected.

**Tech Stack:** Go 1.26, `github.com/golang-migrate/migrate/v4` v4.17.1
(already in `go.mod`), `github.com/golang-migrate/migrate/v4/database/clickhouse`
(NEW blank import), `github.com/ClickHouse/clickhouse-go/v2` v2.x (NEW —
both for driver registration with `database/sql` AND for testcontainer
verification queries), `github.com/testcontainers/testcontainers-go/modules/clickhouse`
(NEW). ClickHouse server image: `clickhouse/clickhouse-server:24.8`.

**Spec sections covered (in 13.1):** §6.4 ClickHouse table definitions
(both source tables and materialised views), §17 integration-test layer
discipline.

**NOT covered in 13.1:** ingest pipeline, metric queries, HTTP, reports
module, frontend wiring (all deferred).

**Prerequisites (verified):**
- Plan 00 + 00a foundation — `cmd/migrator` exists with Postgres support.
  Verified by: `cmd/migrator/main.go:142` (`migrate.New(migrationsPath, dsn)`).
- Plan 03 — Postgres migration runner pattern.
  Verified by: `migrations/000001_init.up.sql` and 10 follow-up migrations.
- Plan 12.4 — `_ pgx5` blank-import precedent for adding a new database
  driver. Verified by: `cmd/migrator/main.go:48`.
- ADR-0010 — Postgres+ClickHouse split is Accepted.
  Verified by: `docs/adr/0010-postgres-plus-clickhouse.md`.
- `database.clickhouse.dsn` config key already exists.
  Verified by: `configs/development/config.yaml:41-43`.
- Standing rule "integration tests use `//go:build integration`" applies.
  Verified by: `docs/architecture/04-testing-strategy.md:121-126`.

---

## Context (verify-before-assert per `docs/architecture/09-agent-workflow-improvements.md` #1)

The plan's cross-boundary assertions, with inline citations:

- **`go.mod` does NOT yet have `clickhouse-go/v2`.**
  Verified by: `grep -i clickhouse go.mod` → no matches.
- **`Makefile` has `migrate-up/down/status/create` for Postgres only.**
  Verified by: `grep -nE '^migrate-' Makefile` → 4 matches, all Postgres.
- **`cmd/migrator/main.go` reads `DATABASE_URL` and `MIGRATIONS_PATH` env vars.**
  Verified by: `cmd/migrator/main.go:94-97`.
- **`cmd/migrator/integration_test.go` is gated by `//go:build integration`.**
  Verified by: `cmd/migrator/integration_test.go:1`.
- **The Postgres test pattern uses `tcpostgres.Run(ctx, "postgres:16-alpine", ...)`.**
  Verified by: `cmd/migrator/integration_test.go:36-46`.
- **`golang-migrate/migrate/v4/database/clickhouse` exists and accepts DSN
  `clickhouse://host:port?database=…&x-multi-statement=true`.**
  Verified by: context7 `/golang-migrate/migrate` query — official
  README at `https://github.com/golang-migrate/migrate/tree/master/database/clickhouse`.
- **`testcontainers-go/modules/clickhouse.Run(ctx, image, opts...)`
  returns `*ClickHouseContainer` with `ConnectionString(ctx, params...)`.**
  Verified by: context7 `/testcontainers/testcontainers-go` query —
  official docs at `https://github.com/testcontainers/testcontainers-go/tree/main/modules/clickhouse`.
- **Migration filenames use project-standard `000NNN_<name>.{up,down}.sql`.**
  Verified by: `ls migrations/` — `000001_init.up.sql` … `000011_admin_grants_call_recordings.up.sql`.

**Assumed (design choices made in 13.1, not facts):**
- ClickHouse 24.8 image is the test target (matches Yandex Managed CH
  supported releases — verify against Yandex docs at execution time).
- CH single-node deployment is OK for v1 (cluster mode is a Plan 01 concern).
- `events_recording_uploaded` schema is locked-in here; the corresponding
  NATS subject is resolved in Plan 13.2 (see references Q4).

---

## File Structure

```
cmd/migrator/
├── main.go                      # MODIFY: add --target flag, blank-import CH driver, env-var routing
├── main_test.go                 # MODIFY: add argv parsing tests for --target=clickhouse
├── ch.go                        # NEW: ClickHouse-specific glue (env-var names, default migrations path)
├── ch_test.go                   # NEW: unit tests for ch.go helpers
├── integration_test.go          # NO CHANGE (existing Postgres tests stay as-is)
└── integration_ch_test.go       # NEW: CH testcontainer + migrate-up + schema-shape + MV-rollup + parity tests

migrations/clickhouse/                             # NEW directory
├── 000001_events_calls.up.sql                     # NEW
├── 000001_events_calls.down.sql                   # NEW
├── 000002_events_operator_state.up.sql            # NEW
├── 000002_events_operator_state.down.sql          # NEW
├── 000003_events_recording_uploaded.up.sql        # NEW
├── 000003_events_recording_uploaded.down.sql      # NEW
├── 000004_mv_calls_hourly.up.sql                  # NEW
├── 000004_mv_calls_hourly.down.sql                # NEW
├── 000005_mv_operator_kpi_daily.up.sql            # NEW
├── 000005_mv_operator_kpi_daily.down.sql          # NEW
├── 000006_mv_quotas_progress.up.sql               # NEW
└── 000006_mv_quotas_progress.down.sql             # NEW

docs/architecture/
└── analytics-mv.md              # NEW: read pattern documentation (sumMerge, FINAL semantics, when raw vs MV)

Makefile                          # MODIFY: add migrate-ch-up/down/status/create targets

go.mod / go.sum                   # MODIFY: + clickhouse-go/v2, + testcontainers-go/modules/clickhouse
```

Total new code: ~250 LoC SQL (CH schema) + ~400 LoC Go (migrator
extension + integration tests) + ~100 LoC docs (analytics-mv.md).

**Path-correction note:** all paths above are verified against the
current scaffolding. `cmd/migrator/` and `migrations/` are project
roots (not under `pkg/` or `internal/`).

---

## Conventions and global rules (Plan 13.1)

Read this once. Apply throughout.

- **Tenant-id is the first ORDER BY column on every source table** —
  always. Materialised views inherit this discipline.
- **No PII in any column.** All identifiers are UUIDs or
  `LowCardinality(String)` enums. Phone numbers, emails, names — never.
- **TTL = `date + INTERVAL 26 MONTH`** on every source table — fixed
  for v1. A future plan may make this tenant-tunable.
- **`LowCardinality(String)`** for fixed-vocabulary columns: `status`,
  `region_code`, `hangup_cause`, `state`, `trunk_used`, `fs_node`,
  `encryption_key_alias`. All comfortably under the soft 10k-unique cap
  (see `docs/references/plan-13-analytics.md` § Gotchas).
- **`event_id UUID`** carried through every event for future de-dup.
- **`_inserted_at DateTime DEFAULT now()`** on every source table —
  records ingest-time stamp (debug + future ReplacingMergeTree pattern).
- **Migrations are idempotent.** Every `CREATE TABLE` uses
  `IF NOT EXISTS`. Every `CREATE [MATERIALIZED] VIEW` uses
  `IF NOT EXISTS`. Re-running `migrator up` against an already-applied
  schema is a no-op (the migrator itself enforces this via
  `schema_migrations`, but the SQL is also belt-and-braces).
- **CH migrations are NOT transactional** (see references § Gotchas).
  Keep each migration to ONE logical change. State table + materialised
  view in a single MV migration is acceptable because both are
  necessary together; partial application is recoverable via
  `migrator force <prev>`.
- **Build tag `//go:build integration`** on every test that needs
  Docker. Run via `go test -tags=integration -race -count=1 ./...`.
- **Pre-commit gate (per task, BEFORE `git commit`):**
  ```
  make ci                          # lint + vet + grep-time-after + test (no -race in CI)
  go test -race -count=1 ./...     # local race detector pass
  go test -tags=integration -race -count=1 ./cmd/migrator/...    # CH testcontainer suite
  gofmt -l .                       # any output = unformatted
  make build                       # all binaries compile
  ```

---

## Tasks

### Task 1: Extend `cmd/migrator` for ClickHouse target

**Goal:** add a `--target=clickhouse` flag (defaulting to `postgres`)
that routes the migration runner to a CH DSN + CH migrations dir.
Three commits:

1. argv-parsing tests + flag plumbing.
2. CH driver blank import + integration test (stub migration in tempdir).
3. Makefile targets + `go get` deps + go.mod/sum.

**Files:**
- Create: `cmd/migrator/ch.go`
- Create: `cmd/migrator/ch_test.go`
- Create: `cmd/migrator/integration_ch_test.go`
- Modify: `cmd/migrator/main.go`
- Modify: `cmd/migrator/main_test.go`
- Modify: `Makefile`
- Modify: `go.mod`, `go.sum`

#### 1.1 — RED: failing argv test for `--target=clickhouse`

- [ ] **Step 1: Write the failing test** (`cmd/migrator/ch_test.go`).

    The test pins the target-resolution logic: a new helper
    `resolveTarget(args []string)` returns `(target string, leftover []string, error)`.
    `target` is one of `"postgres"` or `"clickhouse"`. Default is
    `"postgres"`. Unknown values return a `*usageError`.

    ```go
    package main

    import (
        "errors"
        "testing"
    )

    func TestResolveTarget_DefaultsToPostgres(t *testing.T) {
        t.Parallel()
        target, rest, err := resolveTarget([]string{"up"})
        if err != nil {
            t.Fatalf("unexpected err: %v", err)
        }
        if target != "postgres" {
            t.Fatalf("expected postgres, got %q", target)
        }
        if len(rest) != 1 || rest[0] != "up" {
            t.Fatalf("expected [up], got %v", rest)
        }
    }

    func TestResolveTarget_AcceptsClickHouse(t *testing.T) {
        t.Parallel()
        target, rest, err := resolveTarget([]string{"--target=clickhouse", "up"})
        if err != nil {
            t.Fatalf("unexpected err: %v", err)
        }
        if target != "clickhouse" {
            t.Fatalf("expected clickhouse, got %q", target)
        }
        if len(rest) != 1 || rest[0] != "up" {
            t.Fatalf("expected [up], got %v", rest)
        }
    }

    func TestResolveTarget_RejectsUnknown(t *testing.T) {
        t.Parallel()
        _, _, err := resolveTarget([]string{"--target=mysql", "up"})
        var ue *usageError
        if !errors.As(err, &ue) {
            t.Fatalf("expected *usageError, got %v", err)
        }
    }

    func TestResolveTarget_FlagAfterSubcommand(t *testing.T) {
        t.Parallel()
        // --target= can appear before OR after the sub-command.
        target, rest, err := resolveTarget([]string{"up", "--target=clickhouse"})
        if err != nil {
            t.Fatalf("unexpected err: %v", err)
        }
        if target != "clickhouse" {
            t.Fatalf("expected clickhouse, got %q", target)
        }
        if len(rest) != 1 || rest[0] != "up" {
            t.Fatalf("expected [up], got %v", rest)
        }
    }
    ```

- [ ] **Step 2: Run the test, verify it fails for the right reason.**

    ```bash
    go test ./cmd/migrator/... -run TestResolveTarget
    ```

    Expected: `FAIL` with `undefined: resolveTarget` (compilation error).

#### 1.2 — GREEN: implement `resolveTarget` + plumb into `main.go`

- [ ] **Step 3: Create `cmd/migrator/ch.go`** with the helper + a
    handful of CH-specific constants:

    ```go
    // Package main — ClickHouse target support for the migrator.
    //
    // The migrator binary applies golang-migrate migration sets to two
    // possible targets:
    //
    //   - postgres  (default) — DATABASE_URL + MIGRATIONS_PATH env vars.
    //   - clickhouse           — CLICKHOUSE_DSN + CLICKHOUSE_MIGRATIONS_PATH.
    //
    // Selection is driven by a --target=<name> CLI flag (default postgres
    // for backward compat). Each target uses its own env-var pair so a
    // single Helm Job can reuse the same binary with different args/env.
    //
    // ClickHouse-specific blank imports register the migrate driver and
    // the database/sql driver; both are init()-only.
    package main

    import (
        // Blank import: registers golang-migrate's "clickhouse" database
        // driver. DSNs in the form clickhouse://host:port?database=...
        // are routed through this driver. Init()-only, no exported symbols
        // we use directly. Required by revive's blank-imports rule.
        _ "github.com/golang-migrate/migrate/v4/database/clickhouse"

        // Blank import: registers the "clickhouse" SQL driver name with
        // database/sql. Required transitively by the migrate driver above
        // (it opens *sql.DB internally). Same init()-only pattern.
        _ "github.com/ClickHouse/clickhouse-go/v2"
    )

    // ClickHouse-specific env var names. These are deliberately distinct
    // from DATABASE_URL / MIGRATIONS_PATH so a misconfigured deployment
    // (forgetting --target=clickhouse but setting CLICKHOUSE_DSN) fails
    // loudly with "DATABASE_URL is empty" rather than silently mismatching.
    const (
        envClickHouseDSN  = "CLICKHOUSE_DSN"
        envClickHousePath = "CLICKHOUSE_MIGRATIONS_PATH"

        defaultClickHouseMigrationsPath = "file:///etc/sociopulse/migrations/clickhouse"
    )

    const (
        targetPostgres   = "postgres"
        targetClickHouse = "clickhouse"
        flagTargetPrefix = "--target="
    )

    // resolveTarget extracts the --target=<name> flag from args (if
    // present) and returns the target plus the leftover args (without
    // the flag). The flag may appear before or after the sub-command.
    // Default target is "postgres" (preserves backward-compat with the
    // pre-flag Postgres-only invocation).
    //
    // Unknown target values return a *usageError so main() routes them
    // to exit code 1.
    func resolveTarget(args []string) (string, []string, error) {
        target := targetPostgres
        leftover := make([]string, 0, len(args))
        for _, a := range args {
            if v, ok := parseFlag(a, flagTargetPrefix); ok {
                switch v {
                case targetPostgres, targetClickHouse:
                    target = v
                default:
                    return "", nil, &usageError{msg: "unknown --target value: " + v + " (must be postgres or clickhouse)"}
                }
                continue
            }
            leftover = append(leftover, a)
        }
        return target, leftover, nil
    }
    ```

- [ ] **Step 4: Run the test, verify all four cases pass.**

    ```bash
    go test ./cmd/migrator/... -run TestResolveTarget
    ```

    Expected: `PASS`.

- [ ] **Step 5: Modify `cmd/migrator/main.go`** to call `resolveTarget`
    early and route to per-target env-vars:

    Replace the body of `main()` between `defer logger.Sync()` and the
    `if err := run(...)` block with:

    ```go
    target, args, err := resolveTarget(os.Args[1:])
    if err != nil {
        var ue *usageError
        if errors.As(err, &ue) {
            logger.Error("migrator usage error", zap.Error(err))
            fmt.Fprint(os.Stderr, usageText)
            os.Exit(1)
        }
        // resolveTarget never returns non-usage errors; defensive:
        logger.Error("migrator failed", zap.Error(err))
        os.Exit(2)
    }

    var dsn, migPath, dsnEnv, pathEnv, defaultPath string
    switch target {
    case targetClickHouse:
        dsn = os.Getenv(envClickHouseDSN)
        migPath = os.Getenv(envClickHousePath)
        dsnEnv, pathEnv, defaultPath = envClickHouseDSN, envClickHousePath, defaultClickHouseMigrationsPath
    default: // targetPostgres
        dsn = os.Getenv("DATABASE_URL")
        migPath = os.Getenv("MIGRATIONS_PATH")
        dsnEnv, pathEnv, defaultPath = "DATABASE_URL", "MIGRATIONS_PATH", defaultMigrationsPath
    }
    if migPath == "" {
        migPath = defaultPath
    }
    _ = dsnEnv  // reserved for future telemetry (logged on usage error)
    _ = pathEnv

    if err := run(args, dsn, migPath, os.Stdout); err != nil {
        // … existing handling unchanged …
    }
    ```

    Update `usageText` to describe the new flag + env vars:

    ```go
    const usageText = `usage: migrator [--target=postgres|clickhouse] <up|down|status|force> [args]

    Targets:
      postgres (default) — uses DATABASE_URL + MIGRATIONS_PATH.
      clickhouse         — uses CLICKHOUSE_DSN + CLICKHOUSE_MIGRATIONS_PATH.

    Subcommands:
      up                   apply all pending migrations
      down                 revert all migrations (dev/test only)
      down --steps=N       revert exactly N steps
      status               print current version and dirty flag
      force <version>      set version + clear dirty flag (manual recovery)

    Environment (postgres target):
      DATABASE_URL         Postgres DSN (required)
      MIGRATIONS_PATH      file:// URL of the Postgres migrations directory
                           (default: file:///etc/sociopulse/migrations)

    Environment (clickhouse target):
      CLICKHOUSE_DSN              ClickHouse DSN (required)
      CLICKHOUSE_MIGRATIONS_PATH  file:// URL of the CH migrations directory
                                  (default: file:///etc/sociopulse/migrations/clickhouse)

    Exit codes:
      0   success
      1   usage error (bad argv, empty DSN)
      2   migration or connection error
    `
    ```

- [ ] **Step 6: Run `go build ./cmd/migrator/...`** — verify it compiles.

    Expected: clean (no compile errors).

#### 1.3 — RED: integration test against CH testcontainer

- [ ] **Step 7: Add deps via `go get`** (run from repo root):

    ```bash
    go get github.com/ClickHouse/clickhouse-go/v2@latest
    go get github.com/testcontainers/testcontainers-go/modules/clickhouse@latest
    go mod tidy
    ```

    Expected: `go.mod` gains the two modules + their transitives.
    No errors.

- [ ] **Step 8: Create `cmd/migrator/integration_ch_test.go`** with
    the stub-migration test:

    ```go
    //go:build integration

    // Package main integration test for the ClickHouse target.
    // Boots clickhouse-server in a container and drives run() against it.
    //
    // Run: go test -tags=integration -count=1 -timeout 5m ./cmd/migrator/...
    package main

    import (
        "context"
        "database/sql"
        "errors"
        "os"
        "path/filepath"
        "testing"
        "time"

        // Required for sql.Open("clickhouse", dsn) inside this test file
        // (the helper queries the schema_migrations table to verify state).
        _ "github.com/ClickHouse/clickhouse-go/v2"
        "github.com/stretchr/testify/require"
        tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
    )

    const chImage = "clickhouse/clickhouse-server:24.8"

    // startClickHouse boots a clickhouse-server container and returns
    // a DSN suitable for both golang-migrate and database/sql. Caller
    // does not need to terminate; t.Cleanup handles it.
    func startClickHouse(t *testing.T) string {
        t.Helper()
        ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
        defer cancel()

        ch, err := tcclickhouse.Run(ctx, chImage,
            tcclickhouse.WithDatabase("sociopulse_test"),
            tcclickhouse.WithUsername("test"),
            tcclickhouse.WithPassword("test"),
        )
        require.NoError(t, err)
        t.Cleanup(func() { _ = ch.Terminate(context.Background()) })

        // golang-migrate's CH driver wants the standard clickhouse:// DSN
        // PLUS x-multi-statement=true for migrations that contain more
        // than one statement (the MV ones in 13.1 Task 3 do).
        dsn, err := ch.ConnectionString(ctx, "x-multi-statement=true")
        require.NoError(t, err)
        return dsn
    }

    // openCHDB wires database/sql to the clickhouse driver for verification queries.
    func openCHDB(t *testing.T, dsn string) *sql.DB {
        t.Helper()
        db, err := sql.Open("clickhouse", dsn)
        require.NoError(t, err)
        t.Cleanup(func() { _ = db.Close() })
        require.NoError(t, db.Ping())
        return db
    }

    // writeStubCHMigration writes a minimal up/down pair into a temporary
    // directory and returns its file:// URL. Used to exercise run() against
    // the CH target before the real migrations land in Task 2.
    func writeStubCHMigration(t *testing.T) string {
        t.Helper()
        dir := t.TempDir()
        require.NoError(t, os.WriteFile(
            filepath.Join(dir, "000001_stub.up.sql"),
            []byte("CREATE TABLE IF NOT EXISTS stub_table (id UInt64) ENGINE = MergeTree ORDER BY id;\n"),
            0o644,
        ))
        require.NoError(t, os.WriteFile(
            filepath.Join(dir, "000001_stub.down.sql"),
            []byte("DROP TABLE IF EXISTS stub_table;\n"),
            0o644,
        ))
        return "file://" + dir
    }

    func TestRunCH_UpAndStatus_AppliesStubMigration(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        migrationsPath := writeStubCHMigration(t)

        // Drive the CH target end-to-end via run().
        require.NoError(t, run([]string{"up"}, dsn, migrationsPath, os.Stdout))

        db := openCHDB(t, dsn)

        // schema_migrations is created by golang-migrate (default engine TinyLog).
        var version uint64
        var dirty bool
        require.NoError(t, db.QueryRow(`SELECT version, dirty FROM schema_migrations`).
            Scan(&version, &dirty))
        require.Equal(t, uint64(1), version)
        require.False(t, dirty)

        // The stub migration's table exists.
        var count uint64
        require.NoError(t, db.QueryRow(`
            SELECT count() FROM system.tables
            WHERE database = currentDatabase() AND name = 'stub_table'
        `).Scan(&count))
        require.Equal(t, uint64(1), count)
    }

    func TestRunCH_Down_RemovesStubTable(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        migrationsPath := writeStubCHMigration(t)

        require.NoError(t, run([]string{"up"}, dsn, migrationsPath, os.Stdout))
        require.NoError(t, run([]string{"down"}, dsn, migrationsPath, os.Stdout))

        db := openCHDB(t, dsn)
        var count uint64
        require.NoError(t, db.QueryRow(`
            SELECT count() FROM system.tables
            WHERE database = currentDatabase() AND name = 'stub_table'
        `).Scan(&count))
        require.Zero(t, count, "stub_table should be gone after down")
    }

    func TestRunCH_ConnectionError_DistinctFromUsage(t *testing.T) {
        t.Parallel()

        // A syntactically-valid but unreachable DSN should produce a
        // wrapped connect error, NOT a *usageError. main() must exit 2.
        dsn := "clickhouse://nope:nope@127.0.0.1:1?database=nope&x-multi-statement=true"
        migrationsPath := writeStubCHMigration(t)

        err := run([]string{"up"}, dsn, migrationsPath, os.Stdout)
        require.Error(t, err)

        var ue *usageError
        require.False(t, errors.As(err, &ue), "connection error must not be classified as usage")
    }
    ```

- [ ] **Step 9: Run the integration test, verify it fails the right way.**

    ```bash
    go test -tags=integration -count=1 -timeout 5m ./cmd/migrator/... -run TestRunCH
    ```

    With Docker running and BEFORE the CH driver blank-import is wired in
    `ch.go` (it WAS in step 3, so this should already be present — but
    if anything's missing, the failure mode is `"unknown driver:
    clickhouse"` from golang-migrate). If the failure is anything else
    (compile, container boot, network), fix that first.

    Expected after step 3 already added the imports: **PASS**.

- [ ] **Step 10: Run the unit suite to confirm no regressions:**

    ```bash
    go test ./cmd/migrator/... -run TestRun_  # existing PG tests
    go test ./cmd/migrator/... -run TestResolveTarget
    ```

    Expected: both PASS. (`TestRun_*` — existing Postgres integration
    tests — only run with `-tags=integration`, but the unit-level
    coverage of `cmd/migrator/main_test.go` should stay green.)

#### 1.4 — Makefile + commit

- [ ] **Step 11: Add Makefile targets** (place after the existing
    `migrate-create` block, before `grep-time-after`):

    ```makefile
    # ----- ClickHouse migrations (analytics) -----
    # CH migrations live in migrations/clickhouse/. Apply against
    # CLICKHOUSE_DSN; default DSN points at the dev compose stack.
    # The migrator binary auto-applies the multi-statement flag —
    # don't add x-multi-statement=true here (the integration test
    # adds it in the connection string returned by testcontainers).

    CLICKHOUSE_DSN ?= clickhouse://app:devpass@localhost:9000/sociopulse?x-multi-statement=true
    CH_MIGRATIONS_PATH ?= file://$(PWD)/migrations/clickhouse

    .PHONY: migrate-ch-up
    migrate-ch-up: ## Apply all pending CH migrations against $$CLICKHOUSE_DSN
    	CLICKHOUSE_DSN='$(CLICKHOUSE_DSN)' CLICKHOUSE_MIGRATIONS_PATH='$(CH_MIGRATIONS_PATH)' \
    	  $(GO) run ./cmd/migrator --target=clickhouse up

    .PHONY: migrate-ch-down
    migrate-ch-down: ## Revert all CH migrations (DEV ONLY)
    	CLICKHOUSE_DSN='$(CLICKHOUSE_DSN)' CLICKHOUSE_MIGRATIONS_PATH='$(CH_MIGRATIONS_PATH)' \
    	  $(GO) run ./cmd/migrator --target=clickhouse down

    .PHONY: migrate-ch-status
    migrate-ch-status: ## Print the current CH migration version + dirty flag
    	CLICKHOUSE_DSN='$(CLICKHOUSE_DSN)' CLICKHOUSE_MIGRATIONS_PATH='$(CH_MIGRATIONS_PATH)' \
    	  $(GO) run ./cmd/migrator --target=clickhouse status

    .PHONY: migrate-ch-create
    migrate-ch-create: ## Create a new CH migration pair: NAME=add_some_table
    	@if [ -z "$(NAME)" ]; then \
    	  echo "ERROR: NAME is required, e.g. make migrate-ch-create NAME=add_some_table"; \
    	  exit 1; \
    	fi
    	@mkdir -p migrations/clickhouse
    	@LAST=$$(ls migrations/clickhouse/ 2>/dev/null | grep -E '^[0-9]+_' | sort -n | tail -1 | grep -oE '^[0-9]+' || echo 0); \
    	 NEXT=$$(printf "%06d" $$((LAST + 1))); \
    	 touch "migrations/clickhouse/$${NEXT}_$(NAME).up.sql" "migrations/clickhouse/$${NEXT}_$(NAME).down.sql"; \
    	 echo "Created migrations/clickhouse/$${NEXT}_$(NAME).{up,down}.sql"
    ```

- [ ] **Step 12: Pre-commit gate.**

    ```bash
    make ci
    go test -race -count=1 ./...
    go test -tags=integration -race -count=1 -timeout 5m ./cmd/migrator/...
    gofmt -l .
    make grep-time-after
    make build
    ```

    All green. (`make ci` = lint + vet + grep-time-after + test, no
    `-race`. The race pass is separate.)

- [ ] **Step 13: Commit Task 1.**

    ```bash
    git add cmd/migrator/main.go cmd/migrator/main_test.go \
            cmd/migrator/ch.go cmd/migrator/ch_test.go \
            cmd/migrator/integration_ch_test.go \
            Makefile go.mod go.sum
    git commit -m "feat(cmd/migrator): add --target=clickhouse flag + CH driver wiring (Plan 13.1 Task 1)

    Adds support for applying golang-migrate migration sets against
    ClickHouse via a new --target=clickhouse CLI flag. Defaults to
    postgres for backward-compat. CH-mode reads CLICKHOUSE_DSN +
    CLICKHOUSE_MIGRATIONS_PATH env vars; PG-mode reads the existing
    DATABASE_URL + MIGRATIONS_PATH (unchanged). Blank-imports
    golang-migrate's clickhouse driver and clickhouse-go/v2 (for the
    database/sql driver name). Adds Makefile targets
    migrate-ch-{up,down,status,create} mirroring the PG ones.

    Integration test cmd/migrator/integration_ch_test.go boots
    clickhouse-server:24.8 via testcontainers-go, applies a stub
    migration, verifies schema_migrations + system.tables. The
    Postgres integration tests in integration_test.go are unchanged.

    Plan: docs/superpowers/plans/2026-05-10-13-1-analytics-clickhouse-schema.md"
    ```

---

### Task 2: Source tables — `events_calls`, `events_operator_state`, `events_recording_uploaded`

**Goal:** ship the three source event tables exactly per master spec
§6.4 (with `events_recording_uploaded` added per references Q4).
Each table has matching `.up.sql` + `.down.sql` migrations and a
schema-shape integration test that asserts engine, partition key,
ordering key, and column types via `system.columns` / `system.tables`.

**Files:**
- Create: `migrations/clickhouse/000001_events_calls.{up,down}.sql`
- Create: `migrations/clickhouse/000002_events_operator_state.{up,down}.sql`
- Create: `migrations/clickhouse/000003_events_recording_uploaded.{up,down}.sql`
- Modify: `cmd/migrator/integration_ch_test.go` — add three schema-shape tests

#### 2.1 — RED: schema-shape test for `events_calls`

- [ ] **Step 1:** Add a helper `applyAllCHMigrations(t, dsn)` to
    `integration_ch_test.go` — boots the testcontainer (already in
    `startClickHouse`), then applies the REAL migrations from
    `migrations/clickhouse/` (file://-relative-to-repo-root):

    ```go
    // applyAllCHMigrations applies every migration in
    // ../../../migrations/clickhouse against the given DSN.
    // Assumes CWD is cmd/migrator/.
    func applyAllCHMigrations(t *testing.T, dsn string) {
        t.Helper()
        // The test runs from cmd/migrator/, so migrations/clickhouse
        // is two directories up.
        absMigrations, err := filepath.Abs(filepath.Join("..", "..", "migrations", "clickhouse"))
        require.NoError(t, err)
        require.NoError(t, run([]string{"up"}, dsn, "file://"+absMigrations, os.Stdout))
    }
    ```

- [ ] **Step 2:** Add the failing schema-shape test:

    ```go
    func TestSchema_EventsCalls_HasExpectedColumns(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        applyAllCHMigrations(t, dsn)
        db := openCHDB(t, dsn)

        // Engine + partition + order key live on system.tables.
        var engine, partitionKey, sortingKey string
        require.NoError(t, db.QueryRow(`
            SELECT engine, partition_key, sorting_key
            FROM system.tables
            WHERE database = currentDatabase() AND name = 'events_calls'
        `).Scan(&engine, &partitionKey, &sortingKey))

        require.Equal(t, "MergeTree", engine)
        require.Equal(t, "toYYYYMM(date)", partitionKey)
        require.Equal(t, "tenant_id, project_id, ts", sortingKey)

        // Column types live on system.columns. Use a map for unordered comparison.
        rows, err := db.Query(`
            SELECT name, type FROM system.columns
            WHERE database = currentDatabase() AND table = 'events_calls'
            ORDER BY position
        `)
        require.NoError(t, err)
        defer rows.Close()

        got := map[string]string{}
        for rows.Next() {
            var n, ty string
            require.NoError(t, rows.Scan(&n, &ty))
            got[n] = ty
        }
        require.NoError(t, rows.Err())

        want := map[string]string{
            "date":         "Date",
            "ts":           "DateTime64(3)",
            "tenant_id":    "UUID",
            "project_id":   "UUID",
            "operator_id":  "UUID",
            "call_id":      "UUID",
            "status":       "LowCardinality(String)",
            "duration_sec": "UInt32",
            "hangup_cause": "LowCardinality(String)",
            "region_code":  "LowCardinality(String)",
            "attempt_no":   "UInt8",
            "trunk_used":   "LowCardinality(String)",
            "event_id":     "UUID",
            "_inserted_at": "DateTime",
        }
        require.Equal(t, want, got)
    }
    ```

- [ ] **Step 3: Run the test, verify it fails for the right reason.**

    ```bash
    go test -tags=integration -count=1 -timeout 5m ./cmd/migrator/... -run TestSchema_EventsCalls
    ```

    Expected: FAIL — likely `migrate up` fails with "no migrations
    found in file:///.../migrations/clickhouse" (the directory exists
    but is empty). If you see compile errors first, fix those.

#### 2.2 — GREEN: `events_calls` migration

- [ ] **Step 4:** Create `migrations/clickhouse/000001_events_calls.up.sql`:

    ```sql
    CREATE TABLE IF NOT EXISTS events_calls
    (
        date          Date,
        ts            DateTime64(3),
        tenant_id     UUID,
        project_id    UUID,
        operator_id   UUID,
        call_id       UUID,
        status        LowCardinality(String),
        duration_sec  UInt32,
        hangup_cause  LowCardinality(String),
        region_code   LowCardinality(String),
        attempt_no    UInt8,
        trunk_used    LowCardinality(String),
        event_id      UUID,
        _inserted_at  DateTime DEFAULT now()
    )
    ENGINE = MergeTree
    PARTITION BY toYYYYMM(date)
    ORDER BY (tenant_id, project_id, ts)
    TTL date + INTERVAL 26 MONTH
    SETTINGS index_granularity = 8192
    ```

- [ ] **Step 5:** Create `migrations/clickhouse/000001_events_calls.down.sql`:

    ```sql
    DROP TABLE IF EXISTS events_calls
    ```

- [ ] **Step 6: Run the test again, verify PASS.**

    ```bash
    go test -tags=integration -count=1 -timeout 5m ./cmd/migrator/... -run TestSchema_EventsCalls
    ```

    Expected: PASS.

#### 2.3 — RED+GREEN: `events_operator_state`

- [ ] **Step 7: Add failing test:**

    ```go
    func TestSchema_EventsOperatorState_HasExpectedColumns(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        applyAllCHMigrations(t, dsn)
        db := openCHDB(t, dsn)

        var engine, partitionKey, sortingKey string
        require.NoError(t, db.QueryRow(`
            SELECT engine, partition_key, sorting_key
            FROM system.tables
            WHERE database = currentDatabase() AND name = 'events_operator_state'
        `).Scan(&engine, &partitionKey, &sortingKey))

        require.Equal(t, "MergeTree", engine)
        require.Equal(t, "toYYYYMM(date)", partitionKey)
        require.Equal(t, "tenant_id, user_id, ts", sortingKey)

        rows, err := db.Query(`
            SELECT name, type FROM system.columns
            WHERE database = currentDatabase() AND table = 'events_operator_state'
            ORDER BY position
        `)
        require.NoError(t, err)
        defer rows.Close()

        got := map[string]string{}
        for rows.Next() {
            var n, ty string
            require.NoError(t, rows.Scan(&n, &ty))
            got[n] = ty
        }
        require.NoError(t, rows.Err())

        want := map[string]string{
            "date":                  "Date",
            "ts":                    "DateTime64(3)",
            "tenant_id":             "UUID",
            "user_id":               "UUID",
            "state":                 "LowCardinality(String)",
            "duration_in_state_sec": "UInt32",
            "project_id":            "Nullable(UUID)",
            "event_id":              "UUID",
            "_inserted_at":          "DateTime",
        }
        require.Equal(t, want, got)
    }
    ```

- [ ] **Step 8: Run test, verify FAIL** (table missing).

- [ ] **Step 9: Create `migrations/clickhouse/000002_events_operator_state.up.sql`:**

    ```sql
    CREATE TABLE IF NOT EXISTS events_operator_state
    (
        date                   Date,
        ts                     DateTime64(3),
        tenant_id              UUID,
        user_id                UUID,
        state                  LowCardinality(String),
        duration_in_state_sec  UInt32,
        project_id             Nullable(UUID),
        event_id               UUID,
        _inserted_at           DateTime DEFAULT now()
    )
    ENGINE = MergeTree
    PARTITION BY toYYYYMM(date)
    ORDER BY (tenant_id, user_id, ts)
    TTL date + INTERVAL 26 MONTH
    SETTINGS index_granularity = 8192
    ```

- [ ] **Step 10: Create `migrations/clickhouse/000002_events_operator_state.down.sql`:**

    ```sql
    DROP TABLE IF EXISTS events_operator_state
    ```

- [ ] **Step 11: Run test, verify PASS.**

#### 2.4 — RED+GREEN: `events_recording_uploaded`

- [ ] **Step 12: Add failing test:**

    ```go
    func TestSchema_EventsRecordingUploaded_HasExpectedColumns(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        applyAllCHMigrations(t, dsn)
        db := openCHDB(t, dsn)

        var engine, partitionKey, sortingKey string
        require.NoError(t, db.QueryRow(`
            SELECT engine, partition_key, sorting_key
            FROM system.tables
            WHERE database = currentDatabase() AND name = 'events_recording_uploaded'
        `).Scan(&engine, &partitionKey, &sortingKey))

        require.Equal(t, "MergeTree", engine)
        require.Equal(t, "toYYYYMM(date)", partitionKey)
        require.Equal(t, "tenant_id, ts", sortingKey)

        rows, err := db.Query(`
            SELECT name, type FROM system.columns
            WHERE database = currentDatabase() AND table = 'events_recording_uploaded'
            ORDER BY position
        `)
        require.NoError(t, err)
        defer rows.Close()

        got := map[string]string{}
        for rows.Next() {
            var n, ty string
            require.NoError(t, rows.Scan(&n, &ty))
            got[n] = ty
        }
        require.NoError(t, rows.Err())

        want := map[string]string{
            "date":                  "Date",
            "ts":                    "DateTime64(3)",
            "tenant_id":             "UUID",
            "project_id":            "UUID",
            "call_id":               "UUID",
            "fs_node":               "LowCardinality(String)",
            "s3_key":                "String",
            "size_bytes":            "UInt64",
            "duration_sec":          "UInt32",
            "encryption_key_alias":  "LowCardinality(String)",
            "event_id":              "UUID",
            "_inserted_at":          "DateTime",
        }
        require.Equal(t, want, got)
    }
    ```

- [ ] **Step 13: Run test, verify FAIL.**

- [ ] **Step 14: Create `migrations/clickhouse/000003_events_recording_uploaded.up.sql`:**

    ```sql
    CREATE TABLE IF NOT EXISTS events_recording_uploaded
    (
        date                  Date,
        ts                    DateTime64(3),
        tenant_id             UUID,
        project_id            UUID,
        call_id               UUID,
        fs_node               LowCardinality(String),
        s3_key                String,
        size_bytes            UInt64,
        duration_sec          UInt32,
        encryption_key_alias  LowCardinality(String),
        event_id              UUID,
        _inserted_at          DateTime DEFAULT now()
    )
    ENGINE = MergeTree
    PARTITION BY toYYYYMM(date)
    ORDER BY (tenant_id, ts)
    TTL date + INTERVAL 26 MONTH
    SETTINGS index_granularity = 8192
    ```

- [ ] **Step 15: Create `migrations/clickhouse/000003_events_recording_uploaded.down.sql`:**

    ```sql
    DROP TABLE IF EXISTS events_recording_uploaded
    ```

- [ ] **Step 16: Run test, verify PASS.**

#### 2.5 — Idempotency check + commit

- [ ] **Step 17: Add an idempotency test to confirm re-running `up`
    is a no-op:**

    ```go
    func TestRunCH_UpIsIdempotent(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        applyAllCHMigrations(t, dsn)

        // Re-running up should be a no-op (migrate.ErrNoChange swallowed
        // by run()), version unchanged.
        applyAllCHMigrations(t, dsn)

        db := openCHDB(t, dsn)
        var version uint64
        var dirty bool
        require.NoError(t, db.QueryRow(`SELECT version, dirty FROM schema_migrations`).
            Scan(&version, &dirty))
        require.Equal(t, uint64(3), version, "expected version=3 after applying 000001..000003")
        require.False(t, dirty)
    }
    ```

- [ ] **Step 18: Pre-commit gate.**

    ```bash
    make ci
    go test -race -count=1 ./...
    go test -tags=integration -race -count=1 -timeout 10m ./cmd/migrator/...
    gofmt -l .
    make grep-time-after
    make build
    ```

    Expected: all green.

- [ ] **Step 19: Commit Task 2.**

    ```bash
    git add migrations/clickhouse/000001_events_calls.up.sql \
            migrations/clickhouse/000001_events_calls.down.sql \
            migrations/clickhouse/000002_events_operator_state.up.sql \
            migrations/clickhouse/000002_events_operator_state.down.sql \
            migrations/clickhouse/000003_events_recording_uploaded.up.sql \
            migrations/clickhouse/000003_events_recording_uploaded.down.sql \
            cmd/migrator/integration_ch_test.go
    git commit -m "feat(migrations/clickhouse): events_calls, events_operator_state, events_recording_uploaded (Plan 13.1 Task 2)

    Three MergeTree source tables for analytics ingest, per master spec
    §6.4 + Plan 13 references Q4 (events_recording_uploaded is added
    here for the QC-report use case in Plan 13.3).

    All three: PARTITION BY toYYYYMM(date), TTL = date + INTERVAL
    26 MONTH, _inserted_at DEFAULT now(), event_id UUID for future
    de-dup. ORDER BY tuned per access pattern:
      - events_calls          → (tenant_id, project_id, ts)
      - events_operator_state → (tenant_id, user_id, ts)
      - events_recording_uploaded → (tenant_id, ts)
    LowCardinality(String) for status/region_code/hangup_cause/state/
    trunk_used/fs_node/encryption_key_alias.

    Schema-shape tests in integration_ch_test.go assert engine +
    partition + sorting key + per-column types via system.tables /
    system.columns. Idempotency test verifies re-running 'up' is a
    no-op.

    Plan: docs/superpowers/plans/2026-05-10-13-1-analytics-clickhouse-schema.md"
    ```

---

### Task 3: Materialised views — `mv_calls_hourly`, `mv_operator_kpi_daily`, `mv_quotas_progress`

**Goal:** ship three AggregatingMergeTree state tables + their
materialised-view feeders, exactly per master spec §6.4. The
`mv_operator_kpi_daily` MV has TWO feeders (one from `events_calls`,
one from `events_operator_state`) writing to the same state table —
this is the canonical CH pattern for joining heterogeneous source
streams into a single rollup. Each MV gets a rollup-shape integration
test that inserts a small fixture, runs `OPTIMIZE … FINAL`, queries
via `*Merge` finals, and asserts the rollup matches.

**Files:**
- Create: `migrations/clickhouse/000004_mv_calls_hourly.{up,down}.sql`
- Create: `migrations/clickhouse/000005_mv_operator_kpi_daily.{up,down}.sql`
- Create: `migrations/clickhouse/000006_mv_quotas_progress.{up,down}.sql`
- Modify: `cmd/migrator/integration_ch_test.go` — three rollup tests + a parity test

#### 3.1 — RED+GREEN: `mv_calls_hourly`

- [ ] **Step 1: Add failing test:**

    ```go
    func TestMV_CallsHourly_RollupShape(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        applyAllCHMigrations(t, dsn)
        db := openCHDB(t, dsn)

        const tenantStr = "11111111-1111-1111-1111-111111111111"
        const projectStr = "22222222-2222-2222-2222-222222222222"

        // Insert 6 calls into events_calls — all in the same hour-bucket,
        // 4 success / 2 fail, region MSK.
        for i := 0; i < 6; i++ {
            status := "success"
            if i >= 4 {
                status = "fail"
            }
            _, err := db.Exec(`
                INSERT INTO events_calls
                (date, ts, tenant_id, project_id, operator_id, call_id, status,
                 duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id)
                VALUES
                (toDate('2026-05-10'), toDateTime64('2026-05-10 12:30:00', 3),
                 toUUID(?), toUUID(?), generateUUIDv4(), generateUUIDv4(), ?,
                 60, 'NORMAL_CLEARING', 'MSK', 1, 'trunk-a', generateUUIDv4())
            `, tenantStr, projectStr, status)
            require.NoError(t, err)
        }

        // Force MV-state merge so the rollup is queryable in one shot.
        _, err := db.Exec(`OPTIMIZE TABLE mv_calls_hourly_state FINAL`)
        require.NoError(t, err)

        // Read via *Merge finals.
        var totalCalls, totalDur uint64
        require.NoError(t, db.QueryRow(`
            SELECT sumMerge(cnt), sumMerge(duration_sec)
            FROM mv_calls_hourly
            WHERE tenant_id = toUUID(?) AND project_id = toUUID(?)
              AND bucket_hour >= toDateTime('2026-05-10 12:00:00')
              AND bucket_hour <  toDateTime('2026-05-10 13:00:00')
        `, tenantStr, projectStr).Scan(&totalCalls, &totalDur))
        require.Equal(t, uint64(6), totalCalls)
        require.Equal(t, uint64(360), totalDur) // 6 calls × 60s each
    }
    ```

- [ ] **Step 2: Run test, verify FAIL** (mv_calls_hourly_state and
    mv_calls_hourly do not exist).

- [ ] **Step 3: Create `migrations/clickhouse/000004_mv_calls_hourly.up.sql`:**

    ```sql
    CREATE TABLE IF NOT EXISTS mv_calls_hourly_state
    (
        tenant_id       UUID,
        project_id      UUID,
        bucket_hour     DateTime,
        status          LowCardinality(String),
        region_code     LowCardinality(String),
        cnt             AggregateFunction(sum, UInt64),
        duration_sec    AggregateFunction(sum, UInt64),
        distinct_calls  AggregateFunction(uniq, UUID)
    )
    ENGINE = AggregatingMergeTree
    PARTITION BY toYYYYMM(bucket_hour)
    ORDER BY (tenant_id, project_id, bucket_hour, status, region_code);

    CREATE MATERIALIZED VIEW IF NOT EXISTS mv_calls_hourly
    TO mv_calls_hourly_state AS
    SELECT
        tenant_id,
        project_id,
        toStartOfHour(ts)                  AS bucket_hour,
        status,
        region_code,
        sumState(toUInt64(1))              AS cnt,
        sumState(toUInt64(duration_sec))   AS duration_sec,
        uniqState(call_id)                 AS distinct_calls
    FROM events_calls
    GROUP BY tenant_id, project_id, bucket_hour, status, region_code
    ```

- [ ] **Step 4: Create `migrations/clickhouse/000004_mv_calls_hourly.down.sql`:**

    ```sql
    DROP VIEW IF EXISTS mv_calls_hourly;
    DROP TABLE IF EXISTS mv_calls_hourly_state
    ```

- [ ] **Step 5: Run test, verify PASS.**

#### 3.2 — RED+GREEN: `mv_operator_kpi_daily` (two-feeder pattern)

- [ ] **Step 6: Add failing test:**

    ```go
    func TestMV_OperatorKpiDaily_AggregatesStatesAndCalls(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        applyAllCHMigrations(t, dsn)
        db := openCHDB(t, dsn)

        const tenantStr = "33333333-3333-3333-3333-333333333333"
        const operatorStr = "44444444-4444-4444-4444-444444444444"
        const projectStr = "55555555-5555-5555-5555-555555555555"

        // Insert 3 calls (2 success, 1 refusal), each 60s.
        for i, status := range []string{"success", "success", "refusal"} {
            _, err := db.Exec(`
                INSERT INTO events_calls
                (date, ts, tenant_id, project_id, operator_id, call_id, status,
                 duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id)
                VALUES
                (toDate('2026-05-10'), toDateTime64('2026-05-10 10:00:00', 3),
                 toUUID(?), toUUID(?), toUUID(?), generateUUIDv4(), ?,
                 60, 'NORMAL_CLEARING', 'MSK', 1, 'trunk-a', generateUUIDv4())
            `, tenantStr, projectStr, operatorStr, status)
            require.NoError(t, err)
            _ = i
        }

        // Insert two operator_state rows: 600s in_call, 300s pause.
        _, err := db.Exec(`
            INSERT INTO events_operator_state
            (date, ts, tenant_id, user_id, state, duration_in_state_sec, project_id, event_id)
            VALUES
            (toDate('2026-05-10'), toDateTime64('2026-05-10 10:00:00', 3),
             toUUID(?), toUUID(?), 'in_call', 600, toUUID(?), generateUUIDv4()),
            (toDate('2026-05-10'), toDateTime64('2026-05-10 10:30:00', 3),
             toUUID(?), toUUID(?), 'pause',   300, toUUID(?), generateUUIDv4())
        `, tenantStr, operatorStr, projectStr, tenantStr, operatorStr, projectStr)
        require.NoError(t, err)

        _, err = db.Exec(`OPTIMIZE TABLE mv_operator_kpi_daily_state FINAL`)
        require.NoError(t, err)

        var calls, success, refusal, talk, pause uint64
        require.NoError(t, db.QueryRow(`
            SELECT sumMerge(calls_total),
                   sumMerge(calls_success),
                   sumMerge(calls_refusal),
                   sumMerge(talk_sec),
                   sumMerge(pause_sec)
            FROM mv_operator_kpi_daily
            WHERE tenant_id = toUUID(?) AND user_id = toUUID(?)
              AND project_id = toUUID(?)
              AND bucket_date = toDate('2026-05-10')
        `, tenantStr, operatorStr, projectStr).Scan(&calls, &success, &refusal, &talk, &pause))

        require.Equal(t, uint64(3),   calls,   "3 calls inserted")
        require.Equal(t, uint64(2),   success, "2 successes")
        require.Equal(t, uint64(1),   refusal, "1 refusal")
        require.Equal(t, uint64(600), talk,    "600s in_call")
        require.Equal(t, uint64(300), pause,   "300s pause")
    }
    ```

- [ ] **Step 7: Run test, verify FAIL.**

- [ ] **Step 8: Create `migrations/clickhouse/000005_mv_operator_kpi_daily.up.sql`:**

    Three statements: state table + 2 MV feeders.

    ```sql
    CREATE TABLE IF NOT EXISTS mv_operator_kpi_daily_state
    (
        tenant_id      UUID,
        user_id        UUID,
        project_id     UUID,
        bucket_date    Date,
        talk_sec       AggregateFunction(sum, UInt64),
        pause_sec      AggregateFunction(sum, UInt64),
        ready_sec      AggregateFunction(sum, UInt64),
        wrap_sec       AggregateFunction(sum, UInt64),
        calls_total    AggregateFunction(sum, UInt64),
        calls_success  AggregateFunction(sum, UInt64),
        calls_refusal  AggregateFunction(sum, UInt64)
    )
    ENGINE = AggregatingMergeTree
    PARTITION BY toYYYYMM(bucket_date)
    ORDER BY (tenant_id, user_id, project_id, bucket_date);

    CREATE MATERIALIZED VIEW IF NOT EXISTS mv_operator_kpi_daily_calls
    TO mv_operator_kpi_daily_state AS
    SELECT
        tenant_id,
        operator_id                                              AS user_id,
        project_id,
        toDate(ts)                                               AS bucket_date,
        sumState(toUInt64(0))                                    AS talk_sec,
        sumState(toUInt64(0))                                    AS pause_sec,
        sumState(toUInt64(0))                                    AS ready_sec,
        sumState(toUInt64(0))                                    AS wrap_sec,
        sumState(toUInt64(1))                                    AS calls_total,
        sumState(if(status = 'success', toUInt64(1), toUInt64(0))) AS calls_success,
        sumState(if(status = 'refusal', toUInt64(1), toUInt64(0))) AS calls_refusal
    FROM events_calls
    GROUP BY tenant_id, user_id, project_id, bucket_date;

    CREATE MATERIALIZED VIEW IF NOT EXISTS mv_operator_kpi_daily_states
    TO mv_operator_kpi_daily_state AS
    SELECT
        tenant_id,
        user_id,
        coalesce(project_id, toUUID('00000000-0000-0000-0000-000000000000')) AS project_id,
        toDate(ts)                                               AS bucket_date,
        sumState(if(state = 'in_call', toUInt64(duration_in_state_sec), toUInt64(0))) AS talk_sec,
        sumState(if(state = 'pause',   toUInt64(duration_in_state_sec), toUInt64(0))) AS pause_sec,
        sumState(if(state = 'ready',   toUInt64(duration_in_state_sec), toUInt64(0))) AS ready_sec,
        sumState(if(state = 'wrap_up', toUInt64(duration_in_state_sec), toUInt64(0))) AS wrap_sec,
        sumState(toUInt64(0))                                    AS calls_total,
        sumState(toUInt64(0))                                    AS calls_success,
        sumState(toUInt64(0))                                    AS calls_refusal
    FROM events_operator_state
    GROUP BY tenant_id, user_id, project_id, bucket_date
    ```

- [ ] **Step 9: Create `migrations/clickhouse/000005_mv_operator_kpi_daily.down.sql`:**

    ```sql
    DROP VIEW IF EXISTS mv_operator_kpi_daily_states;
    DROP VIEW IF EXISTS mv_operator_kpi_daily_calls;
    DROP TABLE IF EXISTS mv_operator_kpi_daily_state
    ```

- [ ] **Step 10: Run test, verify PASS.**

#### 3.3 — RED+GREEN: `mv_quotas_progress`

- [ ] **Step 11: Add failing test:**

    ```go
    func TestMV_QuotasProgress_RegionGroupedByDay(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        applyAllCHMigrations(t, dsn)
        db := openCHDB(t, dsn)

        const tenantStr = "66666666-6666-6666-6666-666666666666"
        const projectStr = "77777777-7777-7777-7777-777777777777"

        // 50 calls in MSK: 35 success, 10 fail, 5 refusal.
        // 30 calls in SPB: 20 success, 5 fail, 5 refusal.
        for region, statusCounts := range map[string]map[string]int{
            "MSK": {"success": 35, "fail": 10, "refusal": 5},
            "SPB": {"success": 20, "fail": 5, "refusal": 5},
        } {
            for status, count := range statusCounts {
                for i := 0; i < count; i++ {
                    _, err := db.Exec(`
                        INSERT INTO events_calls
                        (date, ts, tenant_id, project_id, operator_id, call_id, status,
                         duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id)
                        VALUES
                        (toDate('2026-05-10'), toDateTime64('2026-05-10 14:00:00', 3),
                         toUUID(?), toUUID(?), generateUUIDv4(), generateUUIDv4(), ?,
                         60, 'NORMAL_CLEARING', ?, 1, 'trunk-a', generateUUIDv4())
                    `, tenantStr, projectStr, status, region)
                    require.NoError(t, err)
                }
            }
        }

        _, err := db.Exec(`OPTIMIZE TABLE mv_quotas_progress_state FINAL`)
        require.NoError(t, err)

        // Per-region totals.
        rows, err := db.Query(`
            SELECT region_code,
                   sumMerge(success_cnt),
                   sumMerge(fail_cnt),
                   sumMerge(refusal_cnt)
            FROM mv_quotas_progress
            WHERE tenant_id  = toUUID(?)
              AND project_id = toUUID(?)
              AND bucket_date = toDate('2026-05-10')
            GROUP BY region_code
            ORDER BY region_code
        `, tenantStr, projectStr)
        require.NoError(t, err)
        defer rows.Close()

        type row struct {
            region                          string
            success, fail, refusal          uint64
        }
        var got []row
        for rows.Next() {
            var r row
            require.NoError(t, rows.Scan(&r.region, &r.success, &r.fail, &r.refusal))
            got = append(got, r)
        }
        require.NoError(t, rows.Err())

        require.Equal(t, []row{
            {"MSK", 35, 10, 5},
            {"SPB", 20, 5,  5},
        }, got)
    }
    ```

- [ ] **Step 12: Run test, verify FAIL.**

- [ ] **Step 13: Create `migrations/clickhouse/000006_mv_quotas_progress.up.sql`:**

    ```sql
    CREATE TABLE IF NOT EXISTS mv_quotas_progress_state
    (
        tenant_id     UUID,
        project_id    UUID,
        region_code   LowCardinality(String),
        bucket_date   Date,
        success_cnt   AggregateFunction(sum, UInt64),
        fail_cnt      AggregateFunction(sum, UInt64),
        refusal_cnt   AggregateFunction(sum, UInt64),
        other_cnt     AggregateFunction(sum, UInt64)
    )
    ENGINE = AggregatingMergeTree
    PARTITION BY toYYYYMM(bucket_date)
    ORDER BY (tenant_id, project_id, region_code, bucket_date);

    CREATE MATERIALIZED VIEW IF NOT EXISTS mv_quotas_progress
    TO mv_quotas_progress_state AS
    SELECT
        tenant_id,
        project_id,
        region_code,
        toDate(ts)                                                                AS bucket_date,
        sumState(if(status = 'success', toUInt64(1), toUInt64(0)))                AS success_cnt,
        sumState(if(status = 'fail',    toUInt64(1), toUInt64(0)))                AS fail_cnt,
        sumState(if(status = 'refusal', toUInt64(1), toUInt64(0)))                AS refusal_cnt,
        sumState(if(status NOT IN ('success', 'fail', 'refusal'), toUInt64(1), toUInt64(0))) AS other_cnt
    FROM events_calls
    GROUP BY tenant_id, project_id, region_code, bucket_date
    ```

- [ ] **Step 14: Create `migrations/clickhouse/000006_mv_quotas_progress.down.sql`:**

    ```sql
    DROP VIEW IF EXISTS mv_quotas_progress;
    DROP TABLE IF EXISTS mv_quotas_progress_state
    ```

- [ ] **Step 15: Run test, verify PASS.**

#### 3.4 — Parity test (raw vs MV aggregation)

- [ ] **Step 16: Add a parity integration test** that asserts the
    rollup matches a hand-computed direct aggregation on a 100-row
    fixture. This is the most valuable safeguard — it catches MV
    definition typos that schema-shape tests miss.

    ```go
    func TestMV_CallsHourly_RawVsMVParity(t *testing.T) {
        t.Parallel()

        dsn := startClickHouse(t)
        applyAllCHMigrations(t, dsn)
        db := openCHDB(t, dsn)

        const tenantStr = "88888888-8888-8888-8888-888888888888"
        const projectStr = "99999999-9999-9999-9999-999999999999"

        // 100 calls split across 4 hourly buckets and 3 statuses.
        for i := 0; i < 100; i++ {
            hour := i % 4
            status := []string{"success", "fail", "refusal"}[i%3]
            _, err := db.Exec(`
                INSERT INTO events_calls
                (date, ts, tenant_id, project_id, operator_id, call_id, status,
                 duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id)
                VALUES
                (toDate('2026-05-10'),
                 toDateTime64(concat('2026-05-10 ', leftPad(toString(?), 2, '0'), ':00:00'), 3),
                 toUUID(?), toUUID(?), generateUUIDv4(), generateUUIDv4(), ?,
                 ?, 'NORMAL_CLEARING', 'MSK', 1, 'trunk-a', generateUUIDv4())
            `, hour, tenantStr, projectStr, status, 30+i%10)
            require.NoError(t, err)
        }

        _, err := db.Exec(`OPTIMIZE TABLE mv_calls_hourly_state FINAL`)
        require.NoError(t, err)

        // Raw aggregation directly on events_calls.
        var rawCnt, rawDur uint64
        require.NoError(t, db.QueryRow(`
            SELECT count(), sum(toUInt64(duration_sec))
            FROM events_calls
            WHERE tenant_id = toUUID(?) AND project_id = toUUID(?)
              AND date = toDate('2026-05-10')
        `, tenantStr, projectStr).Scan(&rawCnt, &rawDur))

        // MV aggregation via *Merge finals.
        var mvCnt, mvDur uint64
        require.NoError(t, db.QueryRow(`
            SELECT sumMerge(cnt), sumMerge(duration_sec)
            FROM mv_calls_hourly
            WHERE tenant_id = toUUID(?) AND project_id = toUUID(?)
              AND bucket_hour >= toDateTime('2026-05-10 00:00:00')
              AND bucket_hour <  toDateTime('2026-05-11 00:00:00')
        `, tenantStr, projectStr).Scan(&mvCnt, &mvDur))

        require.Equal(t, rawCnt, mvCnt, "MV call count must match raw")
        require.Equal(t, rawDur, mvDur, "MV duration sum must match raw")
        require.Equal(t, uint64(100), mvCnt, "sanity: 100 calls inserted")
    }
    ```

- [ ] **Step 17: Pre-commit gate.**

    ```bash
    make ci
    go test -race -count=1 ./...
    go test -tags=integration -race -count=1 -timeout 15m ./cmd/migrator/...
    gofmt -l .
    make grep-time-after
    make build
    ```

    Expected: all green.

- [ ] **Step 18: Commit Task 3.**

    ```bash
    git add migrations/clickhouse/000004_mv_calls_hourly.up.sql \
            migrations/clickhouse/000004_mv_calls_hourly.down.sql \
            migrations/clickhouse/000005_mv_operator_kpi_daily.up.sql \
            migrations/clickhouse/000005_mv_operator_kpi_daily.down.sql \
            migrations/clickhouse/000006_mv_quotas_progress.up.sql \
            migrations/clickhouse/000006_mv_quotas_progress.down.sql \
            cmd/migrator/integration_ch_test.go
    git commit -m "feat(migrations/clickhouse): three AggregatingMergeTree MVs (Plan 13.1 Task 3)

    mv_calls_hourly        — hourly calls rollup (cnt + duration + uniq)
                              from events_calls.
    mv_operator_kpi_daily  — daily operator KPIs from BOTH events_calls
                              (calls_total/success/refusal) AND
                              events_operator_state (talk/pause/ready/
                              wrap seconds), via two materialised view
                              feeders writing to a shared state table.
    mv_quotas_progress     — daily per-region call counts by status
                              (success/fail/refusal/other) from
                              events_calls — drives the §FR-I region
                              progress dashboard and the quotas report.

    All three target AggregatingMergeTree state tables; reads MUST use
    *Merge finals (sumMerge, uniqMerge). Read pattern documented in
    docs/architecture/analytics-mv.md (Plan 13.1 Task 4).

    Rollup-shape tests insert fixtures, OPTIMIZE TABLE … FINAL, query
    via *Merge, assert match. Parity test compares raw aggregation on
    events_calls against MV aggregation on mv_calls_hourly over a
    100-row fixture — the canonical safeguard against MV definition
    typos.

    Plan: docs/superpowers/plans/2026-05-10-13-1-analytics-clickhouse-schema.md"
    ```

---

### Task 4: Documentation — `docs/architecture/analytics-mv.md`

**Goal:** capture the MV read pattern (sumMerge / uniqMerge / FINAL
semantics + when to use raw vs MV) so Plan 13.2 query authors don't
hand-roll buggy SQL. One small doc + a final pre-commit gate run.

**Files:**
- Create: `docs/architecture/analytics-mv.md`

#### 4.1 — Write the read-pattern doc

- [ ] **Step 1: Create `docs/architecture/analytics-mv.md`:**

    ````markdown
    # Analytics — Materialised View Read Pattern

    > Plan 13.1 ships three AggregatingMergeTree state tables fed by
    > materialised views over `events_calls` and `events_operator_state`.
    > This doc tells you how to READ them. Plan 13.2 metric queries
    > MUST follow the patterns here — direct SELECT on aggregate-
    > function columns returns garbage.

    ## TL;DR

    - Use the `mv_*` views by name; never read the `mv_*_state` table directly.
    - Wrap every aggregate column in `*Merge`: `sumMerge(cnt)`, `uniqMerge(distinct_calls)`.
    - Always include `tenant_id = ?` as the FIRST predicate.
    - Use `OPTIMIZE TABLE mv_*_state FINAL` only in tests; production reads accept
      eventual rollup convergence.
    - When the access pattern requires lookups outside the MV's ORDER BY, fall back
      to the source table (`events_calls` / `events_operator_state`) and pay the
      scan cost knowingly.

    ## The three MVs

    | View | State table | Source(s) | ORDER BY | Aggregates |
    |---|---|---|---|---|
    | `mv_calls_hourly` | `mv_calls_hourly_state` | `events_calls` | `(tenant_id, project_id, bucket_hour, status, region_code)` | `sumState(cnt)`, `sumState(duration_sec)`, `uniqState(distinct_calls)` |
    | `mv_operator_kpi_daily_calls` + `mv_operator_kpi_daily_states` | `mv_operator_kpi_daily_state` | `events_calls` + `events_operator_state` | `(tenant_id, user_id, project_id, bucket_date)` | `sumState(calls_total/success/refusal/talk_sec/pause_sec/ready_sec/wrap_sec)` |
    | `mv_quotas_progress` | `mv_quotas_progress_state` | `events_calls` | `(tenant_id, project_id, region_code, bucket_date)` | `sumState(success_cnt/fail_cnt/refusal_cnt/other_cnt)` |

    ## Canonical read shape

    ```sql
    SELECT
        bucket_hour,
        sumMerge(cnt)                                                       AS total,
        sumMerge(duration_sec)                                              AS dur,
        if(sumMerge(cnt) = 0, 0, sumMerge(duration_sec) / sumMerge(cnt))    AS avg_dur
    FROM mv_calls_hourly
    WHERE tenant_id  = ?
      AND project_id = ?
      AND bucket_hour >= ? AND bucket_hour < ?
    GROUP BY bucket_hour
    ORDER BY bucket_hour;
    ```

    The `WHERE` clause filters before the merge, the `GROUP BY` re-aggregates
    overlapping parts, the `*Merge` finals collapse the AggregateFunction state
    into a scalar.

    **Common mistakes:**

    - `SELECT cnt FROM mv_calls_hourly` — returns binary state bytes, not a number.
    - `SELECT sum(cnt) FROM mv_calls_hourly` — `sum` over `AggregateFunction(sum, UInt64)`
      is undefined; you want `sumMerge`.
    - Forgetting `GROUP BY` when querying a sub-key — overlapping parts leak through
      and the result is over-counted.

    ## Two-feeder MV pattern (operator KPI)

    `mv_operator_kpi_daily_state` is fed by TWO materialised views:

    - `mv_operator_kpi_daily_calls` reads `events_calls`, fills the call-count
      columns (`calls_total`, `calls_success`, `calls_refusal`), zeros the
      duration columns (`talk_sec`, `pause_sec`, `ready_sec`, `wrap_sec`).
    - `mv_operator_kpi_daily_states` reads `events_operator_state`, fills the
      duration columns, zeros the call-count columns.

    Both feeders write to the same state table; `AggregatingMergeTree` merges
    them on the (tenant_id, user_id, project_id, bucket_date) key. Reads via
    `mv_operator_kpi_daily` see the merged sum.

    **Caveat:** the operator-state feeder uses
    `coalesce(project_id, toUUID('00000000-0000-0000-0000-000000000000'))` to
    handle the source's `Nullable(UUID) project_id` (operators in the "ready"
    state aren't bound to a project). Reads that need to count "operator's
    total ready time across all projects" sum across the all-zeros project_id
    bucket separately.

    ## When to bypass the MV

    Use the source tables directly when:

    1. The access pattern doesn't fit the MV's `ORDER BY` — e.g. "all calls for
       a single operator across many tenants" (cross-tenant queries don't
       happen in our app, but service-owner debug queries do).
    2. You need raw fields not in the MV (e.g. `hangup_cause`, `attempt_no`,
       `trunk_used` are NOT in any MV).
    3. The window is so small the MV's coarse buckets aren't useful (e.g.
       "last 5 minutes" doesn't benefit from hourly rollups).

    Source-table queries are several orders of magnitude slower than MV reads
    on a year-of-data table; reserve them for ad-hoc inspection, not user-facing
    dashboards.

    ## `OPTIMIZE TABLE … FINAL`

    `OPTIMIZE TABLE mv_*_state FINAL` forces an immediate merge of all parts.
    Use:

    - In tests, to make rollups queryable in a single shot after fixture insert.
    - **Never** in production — `FINAL` blocks until the merge completes, can
      take minutes on large tables, and races with ongoing inserts.

    Production reads tolerate the few-second eventual-convergence window
    inherent to AggregatingMergeTree.

    ## Cluster mode (future)

    Plan 01 (infra) brings up replicated CH. The migration path:

    - State tables become `Replicated*MergeTree` with a path/replica stamp.
    - `schema_migrations` table moves to `ReplicatedMergeTree` (or `SharedMergeTree`
      on CH Cloud) via `x-migrations-table-engine` + `x-cluster-name` DSN params.
    - Materialised view definitions are replicated automatically once they're
      on a replicated table.

    Until then: single-node CH, single-replica state, no cluster keywords in
    migrations.

    ## Cross-references

    - `migrations/clickhouse/000004..000006_*.up.sql` — the canonical MV definitions.
    - `cmd/migrator/integration_ch_test.go::TestMV_*` — rollup-shape tests
      with fixture + assertion examples.
    - `docs/references/plan-13-analytics.md` — gotchas (sumMerge, multi-statement,
      LowCardinality cap).
    - Master spec §6.4 — original schema spec.
    - Plan 13.2 (TBD) — `internal/analytics/store/queries/*.sql` will be the
      first real consumer of these read patterns.
    ````

#### 4.2 — Final verification + commit

- [ ] **Step 2: Pre-commit gate (full).**

    ```bash
    make ci
    go test -race -count=1 ./...
    go test -tags=integration -race -count=1 -timeout 15m ./cmd/migrator/...
    gofmt -l .
    make grep-time-after
    make build
    ```

    Expected: all green.

- [ ] **Step 3: Commit Task 4.**

    ```bash
    git add docs/architecture/analytics-mv.md
    git commit -m "docs(architecture): MV read pattern (Plan 13.1 Task 4)

    docs/architecture/analytics-mv.md captures the AggregatingMergeTree
    read pattern — sumMerge/uniqMerge over the three MVs introduced in
    Plan 13.1 Task 3, the two-feeder operator-KPI pattern, the OPTIMIZE
    FINAL caveat, and the cluster-mode migration path for Plan 01.

    Plan 13.2 metric-query authors read this BEFORE writing
    internal/analytics/store/queries/*.sql.

    Plan: docs/superpowers/plans/2026-05-10-13-1-analytics-clickhouse-schema.md"
    ```

---

## Self-review (auto-check before execution per `docs/architecture/09-agent-workflow-improvements.md`)

**1. Spec coverage** (master spec §6.4 + §17 + ADR-0010):

- §6.4 `events_calls` schema: ✅ Task 2.
- §6.4 `events_operator_state` schema: ✅ Task 2.
- §6.4 `events_recording_uploaded` (out-of-spec but in Plan 13): ✅ Task 2.
- §6.4 `mv_calls_hourly`: ✅ Task 3.
- §6.4 `mv_operator_kpi_daily` (with two-feeder pattern): ✅ Task 3.
- §6.4 `mv_quotas_progress`: ✅ Task 3.
- §17 integration tests with testcontainers: ✅ Tasks 1-3.
- §17 build tag `//go:build integration`: ✅ Task 1.
- ADR-0010 OLTP/OLAP split: ✅ honoured (CH only for analytics).
- Out of scope (deferred): ingest pipeline, MetricsQuery, Redis cache,
  HTTP, reports module — all in 13.2/13.3.

**2. Placeholder scan:** none. All steps have concrete code/SQL/commands.
"TBD" appears once — in the deferred Plan 13.2/13.3 production-lessons
slot in `docs/references/plan-13-analytics.md`, not in this plan.

**3. Type/name consistency:**

- `events_calls.tenant_id UUID` — referenced consistently in
  `mv_calls_hourly`, `mv_operator_kpi_daily_calls`, `mv_quotas_progress`.
- `events_operator_state.user_id UUID` (NOT `operator_id`) — feeder MV
  `mv_operator_kpi_daily_states` reads `user_id`; `mv_operator_kpi_daily_calls`
  aliases `events_calls.operator_id AS user_id` to match the state-table key.
  Verified in Task 3 SQL.
- `mv_calls_hourly_state` columns: `cnt`, `duration_sec`, `distinct_calls`
  — consistent across rollup test (Task 3.1) and parity test (Task 3.4).
- `mv_quotas_progress_state` columns: `success_cnt`, `fail_cnt`,
  `refusal_cnt`, `other_cnt` — consistent across migration (Task 3.3) and
  rollup test.
- Migration filenames `000NNN_<name>.{up,down}.sql` consistent across all
  six migrations.

**4. Pre-commit gate:** every task ends with the canonical
`make ci + race + integration + gofmt + grep-time-after + build` block.

**5. Vocabulary:** terms used here (`tenant`, `project`, `operator`,
`call`, `region`, `materialised view`, `LowCardinality`, `MergeTree`)
are either project-domain (per `CONTEXT.md`) or CH-specific (per
ClickHouse docs). No drift.

**6. ADR contradictions:** ADR-0010 governs Postgres+ClickHouse split
and is honoured. ADR-0011 (NATS over Kafka) doesn't apply in 13.1
(no NATS work). ADR-0015 (TDD mandatory) honoured per RED→GREEN
discipline in every task.

**7. References file:** `docs/references/plan-13-analytics.md` exists
(created BEFORE this plan per CLAUDE.md rule #2a). Subagent prompts in
Phase 3 will reference it.

**8. Path verification:**
- `cmd/migrator/` ✅ exists.
- `migrations/` ✅ exists (Postgres set; new `migrations/clickhouse/` is created in Task 1).
- `docs/architecture/` ✅ exists.
- No paths use `internal/X` where `pkg/X` is correct (path-correction rule does not apply — 13.1 doesn't touch `internal/` or `pkg/`).

**9. Re-review heuristic** (per `docs/architecture/09-agent-workflow-improvements.md` #7):
- Task 1 diff is large (~400 LoC) — full 2-stage review.
- Tasks 2 + 3 have 2-stage review per task.
- Task 4 is doc-only — single review (spec compliance) is sufficient
  since it has no behaviour. If reviewer flags wording, controller
  fixes inline + skips re-review.

---

## Amendments: none

> Filled at close-out (Phase 4) if reality diverges from this plan.

# Plan 13 — Analytics + Reports — references

> Curated reading list for Plan 13 (analytics + reports). Plan 13 is split into sub-plans:
>
> - **13.1** — ClickHouse schema foundation (this is the next executable plan).
> - **13.2** — IngestPipeline (NATS JetStream → ClickHouse batch insert) + MetricsQuery + HTTP.
> - **13.3** — Reports module (XLSX/CSV/PDF templates + asynq async jobs + S3).
>
> Read this file BEFORE writing any code on Plan 13. The "Production lessons"
> section is filled at close-out of each sub-plan and is the most valuable
> input for the next sub-plan.

---

## Canonical specs

### Master system-design spec

- `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` (live path in `ARCHITECTURE.md`)
  - **§FR-I** — Functional Requirements for Insights/Analytics + Reports.
  - **§6.3** — `reports_jobs` Postgres table (Plan 13.3).
  - **§6.4** — ClickHouse tables (`events_calls`, `events_operator_state`),
    materialised views (`mv_calls_hourly`, `mv_operator_kpi_daily`,
    `mv_quotas_progress`). Plan 13.1 implements this section + adds the
    out-of-spec `events_recording_uploaded` table needed for §FR-I QC report.
  - **§15.3** — `sociopulse_*` Prometheus metrics namespace.
  - **§17** — test strategy (per-layer coverage + load test thresholds).
  - **§22** — `admin-pages-2.jsx::AdminReports` UI prototype — frontend
    counterpart in Plan 19, NOT this repo.

### ADRs

- `docs/adr/0010-postgres-plus-clickhouse.md` — Accepted. OLTP/OLAP split,
  Postgres for transactional state, ClickHouse for analytical queries.
  ADR-0010 is the authority for "use ClickHouse, not Druid/TimescaleDB".
- `docs/adr/0011-nats-over-kafka.md` — NATS JetStream as the bus carrying
  `dialer.call.finalized`, `operator.state.changed` events into the ingest
  pipeline (Plan 13.2).
- `docs/adr/0013-viper-config.md` — config layering. The
  `database.clickhouse.dsn` key already exists in
  `configs/development/config.yaml` (Plan 13.1 fills it in for testing,
  Plan 13.2/13.3 add ingest+rendering knobs).
- `docs/adr/0015-tdd-mandatory.md` — TDD discipline. Schema-shape tests
  written FIRST in 13.1 (RED → migration → GREEN).

### Architecture docs

- `docs/architecture/04-testing-strategy.md` — integration tests via
  `testcontainers-go`, build tag `//go:build integration`. CH module
  testcontainer = `github.com/testcontainers/testcontainers-go/modules/clickhouse`.
- `docs/architecture/05-configuration.md` — viper layering. CH DSN read
  from `database.clickhouse.dsn` (already declared).
- `docs/architecture/07-go-coding-standards.md` — applies as-is to the
  analytics package once 13.2 lands (`internal/analytics/{api,service,store,events}`).
- `docs/architecture/08-tdd-discipline.md` — RED-GREEN-REFACTOR per task.
  Schema migrations have a TDD-friendly path: write the shape-test first,
  watch it FAIL (table missing), then add the migration, watch it PASS.
- `docs/architecture/09-agent-workflow-improvements.md` — verify-before-assert,
  re-review proportionality, plan amendments. Applies starting from Plan 13.

### 152-ФЗ / compliance posture

- `docs/references/COMMON.md` § Compliance posture — pragmatic, not
  theatrical. Analytics events carry **no PII** (all UUIDs + low-cardinality
  enum strings); event ingest does not trip new compliance concerns.
- Master spec §FR-Y4 — data retention. `events_*` tables in Plan 13.1
  use `TTL date + INTERVAL 26 MONTH` to align with the audit-trail
  retention window.

---

## Reference implementations

### ClickHouse Go driver — `github.com/ClickHouse/clickhouse-go/v2`

- **Source repo:** https://github.com/ClickHouse/clickhouse-go
- **context7 ID:** `/clickhouse/clickhouse-go` (verified via `resolve-library-id`).
- **Native open:** `clickhouse.Open(&clickhouse.Options{Addr, Auth, Settings, Compression, ...})` returns `driver.Conn`.
- **database/sql open (used by golang-migrate):** `sql.Open("clickhouse", dsn)` after `_ "github.com/ClickHouse/clickhouse-go/v2"`.
- **Batch insert:** `conn.PrepareBatch(ctx, "INSERT INTO ...")` → `batch.Append(...)` → `batch.Send()`.
- Plan 13.1 uses the **database/sql** path — golang-migrate needs `*sql.DB`
  via `clickhouse.OpenDB(opts)` or via `sql.Open("clickhouse", dsn)`.
- Plan 13.2 will use the **native** path for the high-throughput ingest
  pipeline (`PrepareBatch` + `Send` once per batch is the canonical
  high-perf pattern).

### golang-migrate ClickHouse driver — `github.com/golang-migrate/migrate/v4/database/clickhouse`

- **Source repo:** https://github.com/golang-migrate/migrate/tree/master/database/clickhouse
- **context7 ID:** `/golang-migrate/migrate` (versions: `v4_18_3`).
- **DSN format:** `clickhouse://host:port?database=dbname&username=user&password=password&x-multi-statement=true`.
- **Required query parameters:**
  - `database` — target database (default `default`).
  - `username` / `password` — credentials.
  - `x-multi-statement=true` — required for migrations with multiple
    statements (e.g. MV migrations create both the state table AND the
    materialised view).
- **Optional:**
  - `x-migrations-table` — defaults to `schema_migrations`.
  - `x-migrations-table-engine` — defaults to `TinyLog`. **Keep
    default for v1.** A future cluster-mode plan would switch to
    `ReplicatedMergeTree` and pass `x-cluster-name`.
- **Driver import (blank):** `_ "github.com/golang-migrate/migrate/v4/database/clickhouse"`.
- The Postgres path uses `_ "github.com/golang-migrate/migrate/v4/database/postgres"` plus `_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"` — Plan 13.1 adds the CH driver as a third blank import.

### testcontainers-go ClickHouse module — `github.com/testcontainers/testcontainers-go/modules/clickhouse`

- **Source:** https://github.com/testcontainers/testcontainers-go/tree/main/modules/clickhouse
- **context7 ID:** `/testcontainers/testcontainers-go`.
- **API:**
  - `clickhouse.Run(ctx, "clickhouse/clickhouse-server:24.x", opts...)` returns `*ClickHouseContainer`.
  - `container.ConnectionString(ctx, params...)` returns full DSN
    `clickhouse://default:pass@host:port?...`.
  - `container.ConnectionHost(ctx)` returns `localhost:9000`.
  - `clickhouse.WithUsername("user")`, `clickhouse.WithPassword("pwd")`,
    `clickhouse.WithDatabase("db")`, `clickhouse.WithInitScripts(...)`.
- **Recommended image pin:** `clickhouse/clickhouse-server:24.8` —
  matches Yandex Managed CH supported version (verify against Yandex docs at execution time).
- The existing `cmd/migrator/integration_test.go` uses `tcpostgres.Run`
  with a `wait.ForLog("database system is ready to accept connections")`
  strategy. The CH module ships its own readiness probe — no manual
  wait-strategy needed.

### ClickHouse documentation (read at need)

- **Engines:** https://clickhouse.com/docs/en/engines/table-engines
  - `MergeTree` family — used for source tables.
  - `AggregatingMergeTree` — used for MV state tables. Aggregate
    function columns store partial state (`sumState`, `uniqState`),
    read via `*Merge` finals (`sumMerge`, `uniqMerge`).
- **Materialised views:** https://clickhouse.com/docs/en/sql-reference/statements/create/view
  - `CREATE MATERIALIZED VIEW ... TO target_table AS SELECT ...` —
    Plan 13.1 uses this pattern. Two MVs can target the same state
    table (Plan 13.1 `mv_operator_kpi_daily` does this — calls + states).
- **TTL:** https://clickhouse.com/docs/en/sql-reference/statements/create/table#ttl-expression
  - `TTL date + INTERVAL 26 MONTH` — auto-deletes rows after 26 months.
- **LowCardinality:** https://clickhouse.com/docs/en/sql-reference/data-types/lowcardinality
  - Used for `status`, `region_code`, `hangup_cause`, `state`, `fs_node`,
    `encryption_key_alias`. Saves disk + speeds GROUP BY.

### Existing project patterns to mirror

- **Migration runner:** `cmd/migrator/main.go` — small, env-driven
  (`DATABASE_URL`, `MIGRATIONS_PATH`), zap logger, exit-code contract.
  Plan 13.1 adds a `--target=clickhouse` flag (defaulting to `postgres`).
- **Testcontainer integration test:** `cmd/migrator/integration_test.go`
  — sets up Postgres 16 in a container, drives `run([]string{"up"}, dsn,
  migrationsPath, os.Stdout)` end-to-end. Plan 13.1 adds a sibling
  `integration_ch_test.go` with the same shape but for CH.
- **Build tag:** `//go:build integration` on test files needing Docker.
  Run via `go test -tags=integration ./...` (tooling: testcontainers-go
  is wired in `pkg/postgres`, `pkg/outbox`, `cmd/migrator`).

---

## Gotchas (IMPORTANT — read before coding)

### CH migrations are NOT transactional

`golang-migrate`'s ClickHouse driver does NOT wrap migrations in
transactions (CH's transaction model is limited and the driver does
not use it). Implication:

- If a migration fails halfway through (e.g. CREATE TABLE A succeeds
  but CREATE TABLE B fails), the schema is **partially applied**.
- The migrator marks the migration as `dirty` in `schema_migrations`.
- Manual recovery: `migrator force <prev_version>` + clean up the
  partially-applied objects + retry.
- **Mitigation:** keep each migration to ONE logical change (one
  table or one MV). Plan 13.1 does this — six migrations, each
  ≤ 2 statements.

### `x-multi-statement=true` splits by `;`

The CH driver's multi-statement handling **splits the migration text
by `;`** before executing each piece. Implication:

- A `;` inside a string literal (e.g. a column DEFAULT or COMMENT)
  will break parsing.
- **Mitigation:** Plan 13.1 migrations have no string literals
  containing `;`. Future migrations must avoid them or use
  `x-multi-statement=false` and write one statement per file.

### AggregatingMergeTree: read with `*Merge`, not `*`

A column declared as `cnt AggregateFunction(sum, UInt64)` stores
partial sum state, NOT the final value. Reading it with `sum(cnt)`
gives garbage; you MUST read with `sumMerge(cnt)`.

- Document the read pattern in `docs/architecture/analytics-mv.md`
  (Plan 13.1 Task 4) so Plan 13.2 metric-query authors don't
  hand-roll buggy queries.
- The MV definitions in 13.1 use `sumState`, `uniqState`,
  `argMaxState` to emit partial state. Reads in 13.2 will call the
  matching `sumMerge`, `uniqMerge`, `argMaxMerge`.

### LowCardinality has a soft cap

`LowCardinality(String)` is efficient for ≤ 10k unique values per
column per part. Beyond that, performance degrades to "regular String
plus overhead". Used in 13.1 for:

- `status` — 4 values (`success`, `fail`, `refusal`, …) ✓
- `region_code` — ~90 values (RU subjects) ✓
- `hangup_cause` — ~30 SIP causes ✓
- `state` — 5 values (`ready`, `pause`, `in_call`, `wrap_up`, `offline`) ✓
- `trunk_used` — bounded by tenant's trunk catalogue, ~10s ✓
- `fs_node` — bounded by FS cluster size, ~10s ✓
- `encryption_key_alias` — bounded by KMS key catalogue, ~10s per tenant × 30 tenants = ~300 ✓

All within budget.

### testcontainers-go Docker requirement

`go test -tags=integration ./...` requires a running Docker daemon.
CI has Docker; local dev needs `make dev-up` or a personal Docker
runtime. The test file should `t.Skip("Docker not available")` if
the daemon is unreachable — but testcontainers-go panics with a clear
error message instead, which is acceptable.

### Schema-migrations table engine in cluster mode

When Plan 01 (infra) brings up replicated CH, the
`schema_migrations` table needs to be on `Replicated*MergeTree` (or
`SharedMergeTree` on CH Cloud). Plan 13.1 ships single-node `TinyLog`
(default). The migration path: a future infra plan adds
`x-migrations-table-engine=Replicated*MergeTree&x-cluster-name=...`
to the DSN before first apply on the cluster.

### `events_recording_uploaded` is "out of spec"

The master spec §6.4 lists `events_calls` and `events_operator_state`
but not `events_recording_uploaded`. Plan 13 (existing 1499-line spec)
adds it because the QC report (§FR-I) needs `recording.uploaded` data.
Plan 13.1 ships the schema; the corresponding NATS subject and
ingest are deferred to Plan 13.2/13.3.

**Open question (deferred to 13.2):** does Plan 12.1's
`tenant.<t>.recording.commit` event suffice, or do we need a
separate `tenant.<t>.recording.uploaded` event? The naming difference
suggests separate semantics (commit = transactional moment of
metadata insert; uploaded = optional later per-tenant event). To be
resolved in 13.2 when the ingest pipeline is wired.

### `events_calls` ordering key

Order: `(tenant_id, project_id, ts)`. Implication: queries that
filter on (tenant + project + time-range) are fast; queries that
filter on (tenant + operator) are NOT — operator queries fall back to
the `mv_operator_kpi_daily` materialised view. Document this in
`docs/architecture/analytics-mv.md` (13.1 Task 4).

---

## Open questions (resolved at close-out)

### Plan 13.1

- **Q1.** Migration filename convention — date-prefix (`20260506000010_…`)
  or project-standard `000NNN_…`?
  - **Decision:** project-standard `000001..000006`. Rationale:
    consistency with existing `migrations/000001..000011`.
    The `migrations/clickhouse/` directory is separate, so number
    collisions are impossible.
- **Q2.** Should schema-shape tests live in
  `cmd/migrator/integration_ch_test.go` or
  `internal/analytics/store/ch_schema_test.go`?
  - **Decision:** `cmd/migrator/integration_ch_test.go`. Rationale:
    13.1 doesn't yet create the analytics package (that's 13.2).
    The migrator binary owns the schema; the schema test is a
    natural responsibility of the migrator's integration suite.
    13.2 will add separate `internal/analytics/store/*_test.go` files
    for store-level interactions, NOT schema shape (the schema is
    locked-in by 13.1).
- **Q3.** Image tag — `clickhouse/clickhouse-server:24.8` or `:latest`?
  - **Decision:** `:24.8`. Rationale: matches Yandex Managed CH
    supported versions. `:latest` floats and breaks reproducibility.
    Bump in a separate plan if a real CVE/feature need arises.

### Plan 13.2 (deferred)

- **Q4.** Subject for recording.uploaded — reuse `tenant.<t>.recording.commit`
  or new event? See gotcha above.
- **Q5.** Async insert (`clickhouse.WithAsync(true)`) vs `PrepareBatch`?
  - clickhouse-go v2 supports both; `PrepareBatch` is the canonical
    pattern for high-throughput; `WithAsync` is server-side buffering.
  - Will pick at 13.2 with a benchmark.
- **Q6.** Redis cache invalidation on tenant project rename / delete?
  - The cache key `analytics:{tenant_id}:{q_hash}` is tenant-scoped
    but not project-scoped. Project rename = stale name in cached
    OperatorComparisonRow.DisplayName. Mitigated by short TTL (30s/5min).

### Plan 13.3 (deferred)

- **Q7.** PDF rendering library — `gopdf` (existing spec) or `gofpdf`?
  - To resolve at 13.3 start.
- **Q8.** asynq queue partitioning — per-tenant or global?
  - Global with priority queue; tenant fairness via per-tenant
    `Queue("tenant-X", priority)` if needed. To resolve at 13.3.

---

## Production lessons (post-execution — to be filled by close-out)

> Filled at close-out of each sub-plan. The "next agent on this
> module" reads this section first; it's the highest-leverage doc
> in the file.

### 13.1 (TBD)

### 13.2 (TBD)

### 13.3 (TBD)

---

## Cross-references

- `docs/references/COMMON.md` — cross-cutting (152-ФЗ, Yandex Cloud,
  Postgres, NATS, Outbox).
- `docs/superpowers/plans/2026-05-06-13-analytics-reports.md` —
  the original monolithic Plan 13 spec covering analytics tasks 1-4.
  Plan 13.1 implements tasks 1+2 (schema only); 13.2/13.3 will
  implement tasks 3+ (ingest, queries, reports).
- `docs/architecture/00-overview.md` — analytics block in the system
  diagram.

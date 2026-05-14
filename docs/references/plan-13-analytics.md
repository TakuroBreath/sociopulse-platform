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

### Plan 13.2 (resolved at plan-write 2026-05-14)

- **Q4.** Subject for recording.uploaded — reuse `tenant.<t>.recording.uploaded` (per-tenant)
  or new global subject?
  - **Decision:** subscribe to existing `tenant.*.recording.uploaded` wildcard.
    Rationale: recording already publishes this per-tenant subject (verified
    in `internal/recording/service/service.go` via outbox). Adding a second
    cross-tenant `analytics.recording.uploaded` subject would require a
    double-publish in recording's Commit Tx — extra complexity for no win.
    The analytics ingester extracts `tenant_id` from the subject (token 2)
    rather than relying on payload field. **Caveat:** the existing
    `RecordingUploadedEvent` payload is missing 4 fields the CH
    `events_recording_uploaded` table needs (project_id, fs_node, s3_key,
    encryption_key_alias, event_id). Plan 13.2 Task 1 extends the payload
    additively (backwards-compatible).
- **Q5.** Async insert (`clickhouse.WithAsync(true)`) vs `PrepareBatch`?
  - **Decision:** `PrepareBatch + Append + Send`. Rationale: explicit
    batching gives us deterministic flush boundaries (count or time),
    which is essential for the dedup-LRU eviction policy and for
    pre-shutdown drain. `WithAsync` server-side buffering would
    silently lose in-flight rows on broker disconnect.
  - Library API verified via context7 (`/clickhouse/clickhouse-go`):
    `conn.PrepareBatch(ctx, "INSERT INTO …")` → `batch.Append(col1, col2…)`
    per row → `batch.Send()` finalises. `defer batch.Close()` required
    for resource cleanup. `batch.Flush()` is for long-lived batches —
    Plan 13.2 uses fresh batches per flush, not reused ones.
- **Q6.** Redis cache invalidation on tenant project rename / delete?
  - **Decision:** rely on short TTL (30s short window / 5min long window).
    No active invalidation. Rationale: project rename is rare (admin
    operation, <1/month/tenant in target traffic); cached
    `OperatorComparisonRow.DisplayName` is stale for at most 5min, which
    is acceptable for an admin dashboard. Project DELETE is also rare
    and the dashboard typically re-fetches on navigation; a stale cache
    on a deleted project is harmless (zero rows = zero rows). If
    real-time freshness becomes a requirement, a follow-up plan can wire
    `tenant.<t>.crm.project.deleted` → `del analytics:{tenant_id}:*`
    pattern unlink (Redis SCAN + DEL).
- **Q7.** `analytics.event.calls` and `analytics.event.operator_state`
  cross-tenant subjects — declared in spec §1228-1229 but NOT currently
  emitted by any producer. Who emits and at what point?
  - **Decision:** dialer is the producer (per spec). Two integration
    points needed in this repo:
    1. **`analytics.event.operator_state`** — appended by
       `internal/dialer/fsm/audit.go::appendStateLogAndOutbox` ALONGSIDE
       the existing `tenant.<t>.dialer.op.<op_id>.state` outbox row.
       The new row carries the denormalised state-change with
       `duration_in_state_sec` (= previous state's duration delta).
       The implementer must thread the previous-state timestamp through
       the Machine to compute the delta — the FSM already tracks
       transition timestamps in `operator_state_log` rows; reading the
       PREVIOUS state's ts is the cleanest path.
    2. **`analytics.event.calls`** — emitted on `EventStatusSubmitted`
       FSM transition (operator submits status = call genuinely
       finalised with a known outcome). Plan 13.2 Task 1 adds this hook
       AND the canonical `tenant.<t>.dialer.call.finalized` row (both
       were placeholders in `internal/dialer/api/events.go`).
- **Q8.** `events_calls.hangup_cause` source — FSM commit doesn't have
  it (FreeSWITCH event lifecycle).
  - **Decision:** v1 sets `hangup_cause` to empty-string sentinel.
    Plan 13.2 ingester writes "" when the field is missing on the
    inbound event. Operator-side analytics (talk_sec, etc.) work; the
    SIP-cause breakdown report (FR-I QC section) has reduced fidelity
    until a future plan wires telephony-bridge → analytics. Document
    this in the new operational caveats section of `docs/architecture/analytics-mv.md`
    at close-out.
- **Q9.** `events_calls.region_code` source — FSM commit knows
  respondent's region only if the Machine has been wired with the
  respondent row, which it currently isn't.
  - **Decision:** v1 sets `region_code` to the call's respondent's
    region IFF the FSM has it on the Machine context (currently null
    for most paths). Same fidelity caveat as Q8. The
    `mv_quotas_progress` rollup is partial until respondent data flows
    through; the dashboard's `RegionProgress` query returns rows for
    only the regions that have entries — empty regions are silently
    skipped (no NULLs surface to API consumers).
- **Q10.** Where does `cmd/worker` get a NATS subscriber? Currently
  cmd/worker has no NATS infrastructure (PG-leader-only daemons).
  - **Decision:** add `openNATS` helper to `cmd/worker/main.go` mirroring
    `cmd/api/main.go`'s `openNATS` (returns publisher+subscriber, both
    optional). The IngestPipeline is gated by
    `cfg.Analytics.Enabled && natsSub != nil && chConn != nil` — when
    any is missing, log WARN and skip (mirrors the recording-workers
    skip-on-missing-config pattern). cmd/worker grows a thin NATS
    surface but is still a leader-elected daemon binary at heart.
- **Q11.** Where does the analytics module live for HTTP vs ingest?
  - **Decision:** dual-target.
    - `cmd/api` registers `analytics.Module` which mounts HTTP routes
      AND constructs MetricsQuery (cmd/api needs ONLY query path; no
      ingest).
    - `cmd/worker` constructs IngestPipeline DIRECTLY (no module
      registration); the pipeline is one of several errgroup runners,
      same pattern as `recording.RetentionPass`. The
      `analytics.BuildIngestPipeline(...)` helper lives in
      `internal/analytics/wire/ingest.go` and accepts the bus +
      CH conn + logger.
- **Q12.** `RegionProgress.Plan` field — CH has done counts only;
  project quota plan lives in Postgres.
  - **Decision:** Plan 13.2 returns `done` from CH AND queries
    `crm.api.ProjectService.GetProgress(projectID)` (returns
    `*ProjectProgress`) to fetch `quota.Plan` total. Note: store-layer
    `crm.api.ProjectStorePort.AggregateProgress(tx, projectID)` is a
    sibling method that runs INSIDE a Tx; the analytics port uses the
    service method (no Tx needed; service handles its own Tx lifecycle). The dashboard's RegionProgress slice is
    populated server-side by zipping the two. A `crm.api.ProjectService`
    reference is resolved from `Deps.Locator` at Register time
    (same pattern as realtime resolves auth+crm). When the locator
    has no crm registration (minimal-boot path), `Plan=0` is
    returned with a debug log — frontend treats `Plan=0` as "unknown".

### Plan 13.3 (deferred)

- **Q7.** PDF rendering library — `gopdf` (existing spec) or `gofpdf`?
  - To resolve at 13.3 start.
- **Q8.** asynq queue partitioning — per-tenant or global?
  - Global with priority queue; tenant fairness via per-tenant
    `Queue("tenant-X", priority)` if needed. To resolve at 13.3.

---

## Production lessons (post-execution)

> Filled at close-out of each sub-plan. The "next agent on this
> module" reads this section first; it's the highest-leverage doc
> in the file.

### 13.1 (post-execution 2026-05-10)

The CH ecosystem has a few sharp edges that caught us during execution.
Document them here so the next sub-plan (and Plan 14, and any future
CH work) doesn't re-discover them the hard way.

1. **`;` inside `--` SQL comments breaks the multi-statement splitter
   even though `--` comments are otherwise honoured.** golang-migrate's
   `x-multi-statement=true` splits the migration body by literal `;`
   regardless of comment context. We tripped on this in the Task 3
   NIT-fix commit (`b389a47`) — an inline `--` comment above
   `coalesce(project_id, ...)` containing the text "logout); coalesce"
   fragmented the `CREATE MATERIALIZED VIEW` into two malformed
   statements. CH rejected at parse time. Fix landed in commit
   `6247ad1`. **Rule:** in any `migrations/clickhouse/` migration with
   multiple statements, NO `;` inside any comment text — period.

2. **Two-feeder MVs need an explicit read-side VIEW alias.** When two
   `MATERIALIZED VIEW … TO state_table` feeders share a state table
   (e.g. `mv_operator_kpi_daily_state` is fed by both
   `mv_operator_kpi_daily_calls` and `mv_operator_kpi_daily_states`),
   consumers cannot read from a single feeder and see merged results
   — each feeder only carries the columns it owns, with `sumState(0)`
   for everything else. The canonical read endpoint must be a plain
   `CREATE VIEW canonical_name AS SELECT * FROM state_table`. Plan 13.1
   originally shipped the spec without this 4th statement — caught
   during Task 3 execution; added inline. Plan 13.2 query authors:
   the `mv_*` view names in this file's table are the read endpoints;
   never read from `mv_*_state` or any individual feeder.

3. **CH INSERT VALUES rejects non-constant column expressions.**
   `concat(…)`, `leftPad(…)`, `toString(?)` and similar functions
   inside an `INSERT INTO … VALUES (…)` clause return code 36 "not a
   constant expression" — even when the only "non-constant" element
   is a bound `?` of plain-integer type. Fix: pre-format complex
   literals in the host language before binding, or use `INSERT INTO
   … SELECT` syntax (which DOES allow expressions). We hit this in
   `TestMV_CallsHourly_RawVsMVParity` at execution time; Go-side
   `fmt.Sprintf("2026-05-10 %02d:00:00", i%4)` is the canonical fix
   for tests.

4. **`x-multi-statement=true` is a `golang-migrate`-only DSN
   extension.** `clickhouse-go/v2`'s `sql.Open("clickhouse", dsn)`
   returns "unexpected key 'x-multi-statement'" if the DSN includes
   the flag. The integration suite carries TWO DSNs in a `chDSNs`
   struct: a "migrate-DSN" (with the flag) for `migrate.New` calls,
   and a "verify-DSN" (without the flag) for `database/sql` queries.
   Plan 13.2 will encounter the same — keep the pair.

5. **CH `schema_migrations` engine is `TinyLog` (append-only).**
   Reading the current migration version requires `ORDER BY sequence
   DESC LIMIT 1` — the table accumulates one row per state transition,
   not a single mutable row. golang-migrate's own `Version()` reads
   it this way internally; tests must mirror the pattern.

6. **`gosec G101` trips on illustrative DSN strings in `usageText`.**
   A literal `clickhouse://user:password@host:port/db` in source
   (e.g. inside the migrator's help text) fails the SAST as
   "hardcoded credentials". Replace with descriptive prose ("CH DSN —
   must include `x-multi-statement=true` for multi-statement
   migrations") to preserve operator info without the false-positive.
   The `Makefile`'s `CLICKHOUSE_DSN ?= clickhouse://app:devpass@…`
   default is fine because gitleaks/gosec already allow-list `devpass`
   (matches the existing PG dev-password pattern).

7. **Schema shape can be locked via `system.tables` + `system.columns`
   queries.** This is a clean test pattern that catches column-type
   drift, ORDER BY drift, partition-key drift, and engine drift in
   one shot. The `want map[string]string` in
   `TestSchema_EventsCalls_HasExpectedColumns` (and siblings) is the
   prior-art reference. Beware: ClickHouse normalises type names
   verbosely — `Nullable(UUID)` (parens, not angle brackets),
   `DateTime64(3)` (precision in parens), `LowCardinality(String)`
   (parens), `DateTime` (no precision when `DEFAULT now()` is used
   without explicit precision). Match `want` strings byte-for-byte.

8. **`sorting_key` and `partition_key` are stored as the LITERAL
   expression text from the table definition** — `ORDER BY (a, b, c)`
   becomes `"a, b, c"` (commas + spaces, no parens) in
   `system.tables.sorting_key`; `PARTITION BY toYYYYMM(date)` becomes
   `"toYYYYMM(date)"`. No surprise normalisation.

9. **CI does NOT run `-tags=integration`.** The 8+ testcontainer-based
   tests in `cmd/migrator/integration_ch_test.go` only fire locally
   during pre-commit. This is the documented strategy (per
   `docs/architecture/04-testing-strategy.md`) — testcontainers in
   CI need Docker setup that the project hasn't adopted yet. The
   trade-off: regressions in the CH driver itself only surface
   locally OR in the user's CI. Plan 13.2 should consider adding
   a separate `integration` CI job once the cost of Docker-in-CI
   is justified by the volume of CH-dependent tests.

10. **Per-test CH containers cost ~10s startup but are independent
    and isolated.** With `t.Parallel()` and ~8 cores the integration
    suite finishes in 15-20s wall-time. RAM pressure can produce
    sporadic startup-jitter flakes (one observed during Task 3
    execution); not a Plan 13.1 issue, just an environmental note.
    If CI is going to run integration, allocate ≥8GB to the runner.

### 13.2 (post-execution 2026-05-14)

The producer-consumer pipeline turned out to be the most subject-discipline-heavy work in Plan 13 — most lessons are about NATS subjects, payload field discipline, and the cross-tenant vs per-tenant distinction. Document them here so the next sub-plan (13.3 reports) and Plan 14 (billing) don't re-discover them the hard way.

1. **The `dialer/api.SubjectAnalytics*` constants existed since Plan 13.1 but were never used in production publish code.** The plan template referenced them as if they were the canonical source of truth; verifying via `grep -rn "api\.SubjectAnalytics" --include='*.go'` showed only the new test file referencing them. Task 1's fix-up deleted them; the canonical source of truth is `internal/analytics/api/events.go` (the consumer-side declaration). **Rule:** for cross-tenant analytics subjects, the analytics module owns the constant; producers import it via `analyticsapi "github.com/sociopulse/platform/internal/analytics/api"`. Duplicating the literal in the producer's api/ is a silent-drift trap.

2. **Dialer FSM doesn't have all the fields the CH `events_calls` table needs.** Q8/Q9 caveats — `HangupCause` lives in telephony-bridge's FreeSWITCH event lifecycle; `RegionCode` requires the respondent row which the FSM doesn't carry; `TrunkUsed`/`AttemptNo` similar. v1 emits empty-string sentinels (`""`) which the analytics ingester accepts. A future plan that wires telephony-bridge → analytics will fill them in; until then, the dashboard's calls breakdown is partial-fidelity. Documented in `docs/architecture/analytics-mv.md` § Operational caveats (TODO Plan 13.2 close-out).

3. **`Window.Validate()` returns the bare `ErrInvalidWindow` sentinel — no wrapping.** This is intentional so callers can `errors.Is(err, api.ErrInvalidWindow)` at every layer (service → HTTP handler → JSON envelope). Wrapping it via `fmt.Errorf("ctx: %w", err)` would still survive `errors.Is`, but the bare-sentinel return matches the project's convention for shallow validation errors. Don't wrap unless you have layered diagnostic context to add.

4. **`gin`'s form binding can't decode `uuid.UUID`** — it treats `[16]byte` as a multi-value form array. Symptoms: `["uuid-string"] is not valid value for uuid.UUID` when binding query params with `form:"x" uuid.UUID`. Fix: explicit `parseRequiredUUID(c, "x")` / `parseOptionalUUID(c, "x")` helpers calling `uuid.Parse(c.Query("x"))`. The recording module already established this pattern in `search_handler.go::parseSearchQuery` — re-use, don't reinvent.

5. **Project canonical error envelope is `{ "code": "<stable>", "message": "<human>" }`** per `internal/recording/transport/http.ErrorEnvelope` (matches `pkg/httputil/error_handler.go`). NOT `{ "error": "..." }` (which is `pkg/middleware/auth`'s shape, intentionally different — auth's envelope predates the canonical one and isn't part of the public REST contract). Analytics HTTP handlers mirror the recording shape. Stable `code` values matter — dashboards/log aggregators pivot on them.

6. **`store.Config.{BatchSize, FlushInterval}` are mandatory non-zero** at Validate-time (Task 2 enforced this) but are only used by the ingest path. The query path doesn't batch; cmd/api's `Module.Register` passes `BatchSize=1, FlushInterval=1s` placeholders. If the read-side becomes more common than ingest, consider splitting `store.Config` into `ReadConfig` + `WriteConfig`.

7. **`gzip.Reader.Close` validates the CRC + length trailer.** `io.ReadAll(gz)` alone does NOT consume the trailer — a truncated payload could pass through silently if you `defer gz.Close()` and ignore its error. Always propagate `gz.Close()` from `gunzip`. Same pattern applies to any `compress/zlib`/`compress/flate` reader.

8. **Cache invalidation: TTL-only is the v1 choice (Q6).** Project rename / delete is rare; cached `OperatorComparisonRow.DisplayName` is stale for at most 5min, which is acceptable for an admin dashboard. If real-time freshness becomes a requirement, subscribe to `tenant.<t>.crm.project.deleted` → `DEL analytics:{tenant_id}:*` (Redis SCAN+DEL pattern). The locator-port pattern already in place makes adding an explicit invalidator straightforward.

9. **clickhouse-go/v2's `clickhouse.Open(opts)` Pings during connect ONLY when explicit Ping is called** — the dial is lazy. The wrapper's constructor MUST call `Ping(ctx)` to surface connection failures at boot. Failure path: `_ = drv.Close()` to release the goroutines + return wrapped err. NEVER return a non-nil `*Conn` whose underlying driver hasn't been Ping'd.

10. **`StoreReader` + `StoreReaderAdapter` (Task 4) mirrors `StoreWriter` + `StoreAdapter` (Task 3).** Both ports exist because `store.{Insert,Query}*` are package-level functions, NOT methods on `*store.Conn`. The adapter is a thin wrapper that turns free functions into interface methods. Tests use `fakeStoreReader`/`fakeStoreWriter` satisfying the same port — no real CH needed for the QueryService/IngestPipeline unit tests.

11. **`context.WithoutCancel` is the right primitive for "propagate values but not cancellation"**. Task 3's drain ctx uses `context.WithTimeout(context.WithoutCancel(ctx), DrainTimeout=5s)` — no `//nolint:contextcheck` suppression needed. Same pattern for count-threshold flushes that run inside the bus's push-handler (which carries no ctx). Capture `runCtx` on the struct in Run() before subscribers fire; handlers read it via `context.WithoutCancel(p.runCtx)`.

12. **goleak's `IgnoreTopFunction("internal/poll.runtime_pollWait")` is overly broad.** That's the top-of-stack for ANY netpoll-blocked goroutine — including a leaked clickhouse-go pool connection. Use SPECIFIC top-function names instead: `net/http.(*persistConn).readLoop`, `net/http.(*persistConn).writeLoop` for docker; the testcontainers reaper; quic-go's baseServer.run. Adjust the list when a new pattern surfaces in a future testcontainers bump — never reach for `runtime_pollWait`.

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

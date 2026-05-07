# Analytics + Reports Modules — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use `- [ ]` checkbox syntax. TDD throughout — every behaviour change is preceded by a failing test.

**Goal:** Implement two adjacent modules of СоциоПульс in one coordinated plan: `internal/analytics/` and `internal/reports/`.

- `internal/analytics/` — ingest call/operator events from NATS JetStream into ClickHouse, expose typed metric queries (calls, operator KPIs, region progress, hourly distribution, comparisons), maintain materialised views for fast dashboards. All access is tenant-scoped — `tenant_id` is mandatory in every WHERE clause.
- `internal/reports/` — render the six pre-defined admin reports in XLSX/CSV/PDF, run async export jobs through `asynq` for any custom request and any pre-defined export with `period > 30 days OR rows > 100k`, persist results in S3 with a 24h presigned URL, audit-log every export.

The two modules share queue/cache plumbing and a strict tenant-scoping discipline; combining them into one plan keeps the boundary obvious — `reports/` only consumes `analytics/api`, never the raw CH driver.

**Architecture:** Standard module shape `internal/<module>/{api,service,store,events}` with one extra: `internal/reports/templates/<kind>/{xlsx.go,csv.go,pdf.go}` for renderers. Public surface in `api/`: typed DTOs and interfaces — no third-party types leak. `service/` owns batching, queries, rendering. `store/` — direct ClickHouse driver (analytics) and `pgx` against `reports_jobs` (reports). `events/` — analytics consumes `dialer.call.finalized`/`recording.uploaded`/`operator.state.changed` durably; reports publishes `reports.report.ready`. HTTP endpoints registered through the gateway router from Plan 02 — admin-only RBAC.

**Tech Stack:** Go 1.26+, `github.com/ClickHouse/clickhouse-go/v2` v2.25+, `github.com/xuri/excelize/v2` v2.8+, `github.com/signintech/gopdf` v0.20+, `github.com/hibiken/asynq` v0.24+, `github.com/nats-io/nats.go`, `github.com/redis/go-redis/v9`, `github.com/stretchr/testify`, `github.com/google/uuid`, `github.com/jackc/pgx/v5` (already wired in Plan 03). HTTP via the gateway's gin router from Plan 02. NATS subscription via the shared `events.Bus` interface (Plan 02). CH driver and asynq client are wired from `cmd/api/main.go` once and injected.

**Spec sections covered:** §FR-I (full), §6.4 (ClickHouse tables, materialised views), §6.3 `reports_jobs` table, §15.3 (`sociopulse_*` metrics), §17 (test strategy), §22 prototype `admin-pages-2.jsx::AdminReports`.

**Prerequisites:**
- Plan 00–01 (repo skeleton, infra including ClickHouse, Redis, NATS JetStream, MinIO/S3).
- Plan 02 (`cmd/api` skeleton with config, observability, gateway router, RBAC middleware).
- Plan 03 (Postgres database & migrations runner — the migrator is extended for ClickHouse here).
- Plan 04 (tenancy, RLS, `tenant_settings`).
- Plan 05 (auth — admin RBAC for `/api/reports/*`, `/api/analytics/*`).
- Plan 06 (CRM — `projects` table for filtering).
- Plan 09–10 (operator FSM and dialer — produce the source events that we ingest).
- Plan 11 (audit-log writer) — `reports.export` events go through it.
- Plan 12 (recording — `recording.uploaded` event consumed for storage analytics).
- Plan 14 (billing — `internal/billing/api` cost data referenced by the finance report; only the interface is required).

The plan does not depend on Plan 15 (frontend); the two modules ship a stable HTTP contract and the FE wires up later.

---

## File Structure

```
internal/analytics/
├── api/
│   ├── doc.go                        # package doc — public surface only
│   ├── metrics.go                    # MetricsQuery interface + DTOs (CallsResult, OperatorBreakdown, ...)
│   ├── ingest.go                     # IngestPipeline interface, EventEnvelope DTO
│   ├── errors.go                     # ErrNoData, ErrTenantRequired, ErrInvalidWindow
│   ├── http.go                       # Endpoints(svc Service, mw ...) — registers routes
│   └── http_dto.go                   # response envelopes (HourlyBucket, RegionRow, ...)
│
├── service/
│   ├── service.go                    # Service struct wiring stores, returns api ifaces
│   ├── ingest.go                     # IngestPipeline impl: durable consumer, batching, retry
│   ├── ingest_test.go                # unit + ephemeral CH testcontainer tests
│   ├── ingest_dlq.go                 # dead-letter publishing helpers
│   ├── ingest_dlq_test.go
│   ├── metrics_calls.go              # Calls(ctx, q) → counts/durations/by-status
│   ├── metrics_calls_test.go
│   ├── metrics_operator.go           # OperatorStateBreakdown, OperatorComparisons
│   ├── metrics_operator_test.go
│   ├── metrics_region.go             # RegionProgress (quotas vs done)
│   ├── metrics_region_test.go
│   ├── metrics_hourly.go             # HourlyDistribution
│   ├── metrics_hourly_test.go
│   ├── cache.go                      # Redis cache wrapper (TTL 30s/5min)
│   ├── cache_test.go
│   ├── http_overview.go              # GET /api/analytics/overview
│   ├── http_calls.go                 # GET /api/analytics/calls
│   ├── http_operators.go             # GET /api/analytics/operators
│   ├── http_regions.go               # GET /api/analytics/regions
│   ├── http_hourly.go                # GET /api/analytics/hourly
│   └── http_test.go                  # gateway-level integration: all endpoints
│
├── store/
│   ├── ch.go                         # *clickhouse.Conn wrapper, INSERT batches, query helpers
│   ├── ch_test.go                    # integration tests (testcontainers)
│   ├── pg_quotas.go                  # tiny pgx helper to read quota plans for region progress
│   └── queries/                      # CH SQL kept tidy, embedded by go:embed
│       ├── calls.sql
│       ├── operator_breakdown.sql
│       ├── region_progress.sql
│       ├── hourly.sql
│       └── operator_compare.sql
│
└── events/
    ├── subjects.go                   # const subject names
    ├── consumer.go                   # NATS JetStream consumer (durable)
    ├── consumer_test.go
    └── decoder.go                    # JSON envelope → typed event

internal/reports/
├── api/
│   ├── doc.go
│   ├── kinds.go                      # const kinds: efficiency, project, calls_by_status, finance, qc, hourly, custom
│   ├── jobs.go                       # JobQueue interface, Job DTO
│   ├── reports.go                    # ReportRunner interface, RenderInput, ExportFormat
│   ├── errors.go                     # ErrJobNotFound, ErrFormatUnsupported, ErrAsyncRequired
│   ├── http.go
│   └── http_dto.go
│
├── service/
│   ├── service.go
│   ├── runner.go                     # selects template, renders sync or enqueues
│   ├── runner_test.go
│   ├── jobs.go                       # JobQueue implementation over asynq
│   ├── jobs_test.go
│   ├── consumer.go                   # asynq Server + processor
│   ├── consumer_test.go
│   ├── threshold.go                  # IsAsyncRequired(period, expectedRows)
│   ├── threshold_test.go
│   ├── upload.go                     # S3 upload helpers (presigned URL)
│   ├── upload_test.go
│   ├── audit.go                      # writes audit_log entries
│   ├── http_list.go                  # GET /api/reports
│   ├── http_predefined.go            # POST /api/reports/:kind/export
│   ├── http_custom.go                # POST /api/reports/custom
│   ├── http_jobs.go                  # GET /api/reports/jobs/:jobID, /download
│   └── http_test.go
│
├── store/
│   ├── pg.go                         # reports_jobs CRUD (pgx)
│   ├── pg_test.go
│   └── queries.sql
│
├── templates/
│   ├── common/
│   │   ├── style.go                  # XLSX styles, PDF colour constants
│   │   ├── pdf_layout.go             # PDF helpers (header, footer, page numbering)
│   │   └── csv.go                    # CSV writer with BOM for Excel
│   ├── efficiency/
│   │   ├── data.go                   # query+row shape
│   │   ├── xlsx.go
│   │   ├── csv.go
│   │   ├── pdf.go
│   │   └── render_test.go
│   ├── project_summary/
│   │   ├── data.go
│   │   ├── xlsx.go
│   │   ├── csv.go
│   │   ├── pdf.go
│   │   └── render_test.go
│   ├── calls_by_status/
│   │   ├── data.go
│   │   ├── xlsx.go
│   │   ├── csv.go
│   │   ├── pdf.go
│   │   └── render_test.go
│   ├── finance/
│   │   ├── data.go
│   │   ├── xlsx.go
│   │   ├── csv.go
│   │   ├── pdf.go
│   │   └── render_test.go
│   ├── qc/
│   │   ├── data.go
│   │   ├── xlsx.go
│   │   ├── csv.go
│   │   ├── pdf.go
│   │   └── render_test.go
│   └── hourly_activity/
│       ├── data.go
│       ├── xlsx.go
│       ├── csv.go
│       ├── pdf.go
│       └── render_test.go
│
└── events/
    ├── publisher.go                  # publishes reports.report.ready
    └── publisher_test.go

migrations/clickhouse/
├── 20260506000010_events_calls.up.sql
├── 20260506000010_events_calls.down.sql
├── 20260506000011_events_operator_state.up.sql
├── 20260506000011_events_operator_state.down.sql
├── 20260506000012_events_recording_uploaded.up.sql
├── 20260506000012_events_recording_uploaded.down.sql
├── 20260506000020_mv_calls_hourly.up.sql
├── 20260506000020_mv_calls_hourly.down.sql
├── 20260506000021_mv_operator_kpi_daily.up.sql
├── 20260506000021_mv_operator_kpi_daily.down.sql
├── 20260506000022_mv_quotas_progress.up.sql
└── 20260506000022_mv_quotas_progress.down.sql

cmd/migrator/
└── ch.go                             # MODIFY: register CH driver + run migrations/clickhouse

cmd/api/
└── main.go                           # MODIFY: wire CH conn, asynq client, register both modules

cmd/worker/
└── main.go                           # MODIFY: register reports asynq Server processor

configs/development/config.yaml       # MODIFY: clickhouse: section, reports: section, redis cache TTLs

docs/api/
├── analytics-openapi.yaml            # OpenAPI 3.1 stubs for the 5 analytics endpoints
└── reports-openapi.yaml              # OpenAPI 3.1 stubs for the 5 reports endpoints
```

Total new code: ~6,800 LoC Go, ~250 LoC SQL (CH + materialised views + Postgres helpers), ~400 LoC YAML/OpenAPI. Tests target ≥80% coverage.

---

## Conventions and global rules

Read this once. Repeat in every PR description.

- **Tenant scoping is mandatory.** Every CH query starts with `tenant_id = ?` as the first predicate; the `MetricsQuery` API takes `tenantID` as the first non-context argument and the implementation rejects `uuid.Nil` with `ErrTenantRequired`. The same rule applies to materialised views — they are always grouped by `tenant_id` first.
- **No floats in money or duration aggregates.** Counts are `uint64`, durations seconds are `uint64`, ratios are computed at the end in `float64` only inside the renderer.
- **Time windows are half-open** — `[from, to)`. `from` is inclusive, `to` is exclusive. UI passes ISO timestamps; the service rounds `from` down to its hour and `to` up.
- **Cache only what is read often.** Redis cache key is `analytics:{tenant_id}:{q_hash}` with TTL 30s for live overview, 5 min for region progress and operator comparisons. The hash is `sha256(canonical-json(query))[0:16]`. Cache is bypassed when `from > now-5min` (live tail).
- **Idempotency.** Every CH INSERT carries an `event_id` (UUID from the source NATS message). The MergeTree table doesn't enforce uniqueness — we de-dup with a `ReplacingMergeTree(_inserted_at)` collapsing pattern in the materialised views, plus an in-memory LRU on the consumer for the latest 100k IDs to short-circuit obvious replays.
- **Async threshold.** `service.IsAsyncRequired(period, est)` returns `true` if `period.End.Sub(period.Start) > 30*24h` OR `est > 100_000`. The service refuses to render synchronously above the threshold and forces enqueue.
- **Audit.** Every `POST /api/reports/*/export`, `POST /api/reports/custom`, and `GET /api/reports/jobs/:id/download` writes an `audit_log` row via `internal/audit/api.Writer.Write` (see Plan 11). Subject `reports.export`, payload contains `kind, params, format, job_id?, bytes?`.
- **PDF for big result sets.** PDFs cap at 5,000 rows of detail data; the runner falls back to "summary only" PDF beyond that, and the user gets a 2nd attached XLSX in the same job — surfaced as `result_files` JSON array on the job row.
- **OpenTelemetry.** Each metric query and each render emits a span: `analytics.metrics.calls`, `reports.render.efficiency.xlsx`. Attributes always include `tenant_id`, `format`, `rows_returned`. No PII (phone numbers) in attributes — only IDs.
- **Ports.** `cmd/api` exposes the HTTP API. `cmd/worker` runs the asynq consumer and the analytics ingest pipeline. Both bind to the same config and are deployed as separate `Deployment`s.

---

## Part A — `internal/analytics/`

### Task 1: ClickHouse migrations

Implement the three event-source tables defined in §6.4. Add `events_recording_uploaded` (not in the spec but needed by the QC report and the recording dashboard). Use `golang-migrate` with the `clickhouse://` driver — the `cmd/migrator` binary already initialised in Plan 03 gets a `--clickhouse` flag to run a separate migration set out of `migrations/clickhouse/`.

#### 1.1 — Failing tests first

- [ ] Write `internal/analytics/store/ch_test.go::TestSchema_EventsCalls_HasExpectedColumns` — boots CH testcontainer, runs `migrations/clickhouse/`, asserts:
  - Engine is `MergeTree`.
  - Partition key reads `toYYYYMM(date)`.
  - Ordering key matches `(tenant_id, project_id, ts)`.
  - Columns and types: `date Date`, `ts DateTime64(3)`, `tenant_id UUID`, `project_id UUID`, `operator_id UUID`, `call_id UUID`, `status LowCardinality(String)`, `duration_sec UInt32`, `hangup_cause LowCardinality(String)`, `region_code LowCardinality(String)`, `attempt_no UInt8`, `trunk_used LowCardinality(String)`, `event_id UUID`, `_inserted_at DateTime DEFAULT now()`.

- [ ] Same shape for `TestSchema_EventsOperatorState_HasExpectedColumns` — `(date, ts, tenant_id, user_id, state LowCardinality(String), duration_in_state_sec UInt32, project_id Nullable(UUID), event_id UUID)`. Order by `(tenant_id, user_id, ts)`.

- [ ] `TestSchema_EventsRecordingUploaded_HasExpectedColumns` — `(date, ts, tenant_id, project_id, call_id, fs_node LowCardinality(String), s3_key String, size_bytes UInt64, duration_sec UInt32, encryption_key_alias LowCardinality(String), event_id UUID)`. Order by `(tenant_id, ts)`.

#### 1.2 — Migration files

- [ ] `migrations/clickhouse/20260506000010_events_calls.up.sql`:

```sql
create table if not exists events_calls
(
    date                 Date,
    ts                   DateTime64(3),
    tenant_id            UUID,
    project_id           UUID,
    operator_id          UUID,
    call_id              UUID,
    status               LowCardinality(String),
    duration_sec         UInt32,
    hangup_cause         LowCardinality(String),
    region_code          LowCardinality(String),
    attempt_no           UInt8,
    trunk_used           LowCardinality(String),
    event_id             UUID,
    _inserted_at         DateTime default now()
)
engine = MergeTree
partition by toYYYYMM(date)
order by (tenant_id, project_id, ts)
ttl date + interval 26 month
settings index_granularity = 8192;
```

- [ ] `migrations/clickhouse/20260506000010_events_calls.down.sql`:

```sql
drop table if exists events_calls;
```

- [ ] `migrations/clickhouse/20260506000011_events_operator_state.up.sql`:

```sql
create table if not exists events_operator_state
(
    date                  Date,
    ts                    DateTime64(3),
    tenant_id             UUID,
    user_id               UUID,
    state                 LowCardinality(String),
    duration_in_state_sec UInt32,
    project_id            Nullable(UUID),
    event_id              UUID,
    _inserted_at          DateTime default now()
)
engine = MergeTree
partition by toYYYYMM(date)
order by (tenant_id, user_id, ts)
ttl date + interval 26 month
settings index_granularity = 8192;
```

- [ ] `migrations/clickhouse/20260506000011_events_operator_state.down.sql` — `drop table if exists events_operator_state;`.

- [ ] `migrations/clickhouse/20260506000012_events_recording_uploaded.up.sql`:

```sql
create table if not exists events_recording_uploaded
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
    _inserted_at          DateTime default now()
)
engine = MergeTree
partition by toYYYYMM(date)
order by (tenant_id, ts)
ttl date + interval 26 month
settings index_granularity = 8192;
```

- [ ] `migrations/clickhouse/20260506000012_events_recording_uploaded.down.sql` — `drop table if exists events_recording_uploaded;`.

- [ ] `cmd/migrator/ch.go` — adds a `--clickhouse` flag. Implementation:

```go
package main

import (
    "context"
    "errors"
    "flag"
    "fmt"

    "github.com/golang-migrate/migrate/v4"
    chdriver "github.com/golang-migrate/migrate/v4/database/clickhouse"
    _ "github.com/golang-migrate/migrate/v4/source/file"
    "github.com/ClickHouse/clickhouse-go/v2"
)

func runClickHouseMigrations(ctx context.Context, dsn, dir string) error {
    opts, err := clickhouse.ParseDSN(dsn)
    if err != nil {
        return fmt.Errorf("parse clickhouse dsn: %w", err)
    }
    db := clickhouse.OpenDB(opts)
    defer db.Close()

    drv, err := chdriver.WithInstance(db, &chdriver.Config{
        DatabaseName: opts.Auth.Database,
        MultiStatementEnabled: true,
    })
    if err != nil {
        return fmt.Errorf("clickhouse driver: %w", err)
    }
    m, err := migrate.NewWithDatabaseInstance("file://"+dir, "clickhouse", drv)
    if err != nil {
        return fmt.Errorf("migrate init: %w", err)
    }
    if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
        return fmt.Errorf("migrate up: %w", err)
    }
    return nil
}

func init() {
    flag.Bool("clickhouse", false, "run ClickHouse migrations from migrations/clickhouse")
}
```

The existing `main.go` switches on the flag to call either Postgres or ClickHouse migrations.

#### 1.3 — Wire-up + verification

- [ ] Update `Makefile`: add `migrate-ch` target that runs `go run ./cmd/migrator -clickhouse -dsn $$CLICKHOUSE_DSN -dir migrations/clickhouse`.

- [ ] Update `configs/development/config.yaml`:

```yaml
clickhouse:
  dsn: "clickhouse://default:@localhost:9000/sociopulse?dial_timeout=5s&read_timeout=30s&max_execution_time=60"
  query_timeout: 30s
  insert_timeout: 30s
```

- [ ] Run all tests in §1.1 — they pass.

- [ ] **Verification:** `make migrate-ch` is idempotent on a fresh CH; running twice is a no-op.

---

### Task 2: Materialised views for fast dashboards

Three MVs that pre-aggregate hot data so the overview dashboard doesn't scan a year of raw events. Each MV is `ReplacingMergeTree` so retries from the consumer are absorbed.

#### 2.1 — Failing tests

- [ ] `TestMV_CallsHourly_RollupShape` — inserts a small fixture into `events_calls`, then `OPTIMIZE TABLE mv_calls_hourly FINAL`, asserts `select count() from mv_calls_hourly where tenant_id = ?` equals expected groups.

- [ ] `TestMV_OperatorKpiDaily_AggregatesStatesAndCalls` — fixture covers operator across two days; asserts the row for day-1 has correct `talk_sec`, `pause_sec`, `success_calls`.

- [ ] `TestMV_QuotasProgress_RegionGroupedByDay` — fixture has 50 calls in two regions on the same day, status `success`/`fail`; assert the MV returns one row per (tenant, project, region_code, date).

#### 2.2 — Migrations

- [ ] `migrations/clickhouse/20260506000020_mv_calls_hourly.up.sql`:

```sql
create table if not exists mv_calls_hourly_state
(
    tenant_id      UUID,
    project_id     UUID,
    bucket_hour    DateTime,
    status         LowCardinality(String),
    region_code    LowCardinality(String),
    cnt            AggregateFunction(sum, UInt64),
    duration_sec   AggregateFunction(sum, UInt64),
    distinct_calls AggregateFunction(uniq, UUID)
)
engine = AggregatingMergeTree
partition by toYYYYMM(bucket_hour)
order by (tenant_id, project_id, bucket_hour, status, region_code);

create materialized view if not exists mv_calls_hourly to mv_calls_hourly_state as
select
    tenant_id,
    project_id,
    toStartOfHour(ts)         as bucket_hour,
    status,
    region_code,
    sumState(toUInt64(1))     as cnt,
    sumState(toUInt64(duration_sec)) as duration_sec,
    uniqState(call_id)        as distinct_calls
from events_calls
group by tenant_id, project_id, bucket_hour, status, region_code;
```

- [ ] `migrations/clickhouse/20260506000020_mv_calls_hourly.down.sql`:

```sql
drop view if exists mv_calls_hourly;
drop table if exists mv_calls_hourly_state;
```

- [ ] `migrations/clickhouse/20260506000021_mv_operator_kpi_daily.up.sql`:

```sql
create table if not exists mv_operator_kpi_daily_state
(
    tenant_id           UUID,
    user_id             UUID,
    project_id          UUID,
    bucket_date         Date,
    talk_sec            AggregateFunction(sum, UInt64),
    pause_sec           AggregateFunction(sum, UInt64),
    ready_sec           AggregateFunction(sum, UInt64),
    wrap_sec            AggregateFunction(sum, UInt64),
    calls_total         AggregateFunction(sum, UInt64),
    calls_success       AggregateFunction(sum, UInt64),
    calls_refusal       AggregateFunction(sum, UInt64)
)
engine = AggregatingMergeTree
partition by toYYYYMM(bucket_date)
order by (tenant_id, user_id, project_id, bucket_date);

create materialized view if not exists mv_operator_kpi_daily_calls to mv_operator_kpi_daily_state as
select
    tenant_id,
    operator_id                                          as user_id,
    project_id,
    toDate(ts)                                           as bucket_date,
    sumState(toUInt64(0))                                as talk_sec,
    sumState(toUInt64(0))                                as pause_sec,
    sumState(toUInt64(0))                                as ready_sec,
    sumState(toUInt64(0))                                as wrap_sec,
    sumState(toUInt64(1))                                as calls_total,
    sumState(if(status = 'success', toUInt64(1), toUInt64(0)))  as calls_success,
    sumState(if(status = 'refusal', toUInt64(1), toUInt64(0)))  as calls_refusal
from events_calls
group by tenant_id, user_id, project_id, bucket_date;

create materialized view if not exists mv_operator_kpi_daily_states to mv_operator_kpi_daily_state as
select
    tenant_id,
    user_id,
    coalesce(project_id, toUUID('00000000-0000-0000-0000-000000000000')) as project_id,
    toDate(ts)                                           as bucket_date,
    sumState(if(state = 'in_call',  toUInt64(duration_in_state_sec), toUInt64(0))) as talk_sec,
    sumState(if(state = 'pause',    toUInt64(duration_in_state_sec), toUInt64(0))) as pause_sec,
    sumState(if(state = 'ready',    toUInt64(duration_in_state_sec), toUInt64(0))) as ready_sec,
    sumState(if(state = 'wrap_up',  toUInt64(duration_in_state_sec), toUInt64(0))) as wrap_sec,
    sumState(toUInt64(0))                                as calls_total,
    sumState(toUInt64(0))                                as calls_success,
    sumState(toUInt64(0))                                as calls_refusal
from events_operator_state
group by tenant_id, user_id, project_id, bucket_date;
```

- [ ] `migrations/clickhouse/20260506000021_mv_operator_kpi_daily.down.sql`:

```sql
drop view if exists mv_operator_kpi_daily_calls;
drop view if exists mv_operator_kpi_daily_states;
drop table if exists mv_operator_kpi_daily_state;
```

- [ ] `migrations/clickhouse/20260506000022_mv_quotas_progress.up.sql`:

```sql
create table if not exists mv_quotas_progress_state
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
engine = AggregatingMergeTree
partition by toYYYYMM(bucket_date)
order by (tenant_id, project_id, region_code, bucket_date);

create materialized view if not exists mv_quotas_progress to mv_quotas_progress_state as
select
    tenant_id,
    project_id,
    region_code,
    toDate(ts)            as bucket_date,
    sumState(if(status = 'success', toUInt64(1), toUInt64(0))) as success_cnt,
    sumState(if(status = 'fail',    toUInt64(1), toUInt64(0))) as fail_cnt,
    sumState(if(status = 'refusal', toUInt64(1), toUInt64(0))) as refusal_cnt,
    sumState(if(status not in ('success','fail','refusal'), toUInt64(1), toUInt64(0))) as other_cnt
from events_calls
group by tenant_id, project_id, region_code, bucket_date;
```

- [ ] `migrations/clickhouse/20260506000022_mv_quotas_progress.down.sql` — drops the MV and state table.

#### 2.3 — Verification

- [ ] All tests in §2.1 pass after `OPTIMIZE TABLE ... FINAL` is run.

- [ ] Document the read pattern in `docs/architecture/analytics-mv.md`: **always read with `*Merge` finals**, e.g.:

```sql
select sumMerge(cnt) as calls, sumMerge(duration_sec) as dur
from mv_calls_hourly
where tenant_id = ? and project_id = ? and bucket_hour >= ? and bucket_hour < ?
group by bucket_hour, status;
```

- [ ] Add an integration test that compares "raw aggregation" against "MV aggregation" on a 10k-row fixture; the two must be identical (sanity for the MV definitions).

---

### Task 3: `IngestPipeline` — NATS → CH batch insert

Durable JetStream consumer with in-memory batching, retry+backoff, and a dead-letter subject for poison messages. The consumer runs in `cmd/worker` so the API binary stays light. The pipeline subscribes to three subjects:

- `dialer.call.finalized` → `events_calls`
- `operator.state.changed` → `events_operator_state`
- `recording.uploaded` → `events_recording_uploaded`

Each handler is independent so a slow CH insert on one stream doesn't block the others.

#### 3.1 — Public surface

- [ ] `internal/analytics/api/ingest.go`:

```go
package api

import (
    "context"
    "time"

    "github.com/google/uuid"
)

type EventKind string

const (
    EventKindCallFinalized      EventKind = "dialer.call.finalized"
    EventKindOperatorState      EventKind = "operator.state.changed"
    EventKindRecordingUploaded  EventKind = "recording.uploaded"
)

type EventEnvelope struct {
    EventID   uuid.UUID       `json:"event_id"`
    Kind      EventKind       `json:"kind"`
    TenantID  uuid.UUID       `json:"tenant_id"`
    Timestamp time.Time       `json:"ts"`
    Payload   json.RawMessage `json:"payload"`
}

type IngestPipeline interface {
    // Run blocks until ctx is cancelled. It is idempotent on restart.
    Run(ctx context.Context) error
    // Stats returns runtime counters for /metrics.
    Stats() IngestStats
}

type IngestStats struct {
    PerSubject map[string]SubjectStats
}

type SubjectStats struct {
    Received   uint64
    Inserted   uint64
    Failed     uint64
    DeadLetter uint64
    LagSeconds float64
    LastError  string
}
```

#### 3.2 — Failing tests

- [ ] `internal/analytics/service/ingest_test.go::TestIngest_BatchesAtMaxSize_FlushesBeforeWindow` — feeds 10,001 envelopes into a fake NATS source within 100 ms; asserts the CH adapter saw two `Insert` calls (one with 10,000 rows, one with 1).

- [ ] `TestIngest_FlushesAtTimeWindow` — feeds 100 envelopes spread across 6 seconds; assert at least two inserts, the first at ≈5 s.

- [ ] `TestIngest_RetriesTransient_BackoffExponential` — fake CH adapter returns `ErrTransient` twice, then OK; assert the consumer retries (50ms → 200ms → 800ms with ±20% jitter, capped at 30s) and acks only after success.

- [ ] `TestIngest_PoisonGoesToDLQ_AfterMaxAttempts` — fake CH adapter returns `ErrInvalidPayload` permanently; assert envelope ends up on `dlq.analytics.<kind>` with original headers preserved, NACK'd from the source consumer.

- [ ] `TestIngest_DedupsByEventID_WithinLRU` — feeds the same `event_id` twice; only one CH insert.

- [ ] `TestIngest_SeparateConsumersPerKind_DontBlockEachOther` — slow CH for "operator_state" doesn't slow down "calls" inserts (assert via timing on a fake clock).

#### 3.3 — Implementation

- [ ] `internal/analytics/service/ingest.go`:

```go
package service

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "log/slog"
    "math/rand/v2"
    "sync"
    "time"

    "github.com/google/uuid"
    lru "github.com/hashicorp/golang-lru/v2"
    "github.com/nats-io/nats.go"
    "github.com/nats-io/nats.go/jetstream"

    "github.com/sociopulse/sociopulse/internal/analytics/api"
    "github.com/sociopulse/sociopulse/internal/analytics/store"
)

const (
    maxBatchSize     = 10_000
    maxBatchWindow   = 5 * time.Second
    maxAttempts      = 6
    lruWindow        = 100_000
    minBackoff       = 50 * time.Millisecond
    maxBackoff       = 30 * time.Second
)

type ingestImpl struct {
    js          jetstream.JetStream
    ch          store.Sink
    log         *slog.Logger
    seenIDs     *lru.Cache[uuid.UUID, struct{}]
    statsMu     sync.RWMutex
    statsBySubj map[string]*api.SubjectStats
}

type Sink interface {
    InsertCalls(ctx context.Context, rows []store.CallRow) error
    InsertOperatorStates(ctx context.Context, rows []store.OperatorStateRow) error
    InsertRecordingUploads(ctx context.Context, rows []store.RecordingRow) error
}

var ErrTransient = errors.New("transient ingest error")
var ErrInvalidPayload = errors.New("invalid payload")

func NewIngest(js jetstream.JetStream, sink store.Sink, log *slog.Logger) (api.IngestPipeline, error) {
    seen, err := lru.New[uuid.UUID, struct{}](lruWindow)
    if err != nil {
        return nil, err
    }
    return &ingestImpl{
        js:          js,
        ch:          sink,
        log:         log,
        seenIDs:     seen,
        statsBySubj: map[string]*api.SubjectStats{},
    }, nil
}

func (p *ingestImpl) Run(ctx context.Context) error {
    var wg sync.WaitGroup
    type cfg struct {
        stream   string
        subject  string
        durable  string
        kind     api.EventKind
        flushFn  func(context.Context, []*EnvWithMsg) error
    }
    cfgs := []cfg{
        {"ANALYTICS", "dialer.call.finalized", "analytics-calls",
            api.EventKindCallFinalized, p.flushCalls},
        {"ANALYTICS", "operator.state.changed", "analytics-operator-state",
            api.EventKindOperatorState, p.flushOperatorStates},
        {"ANALYTICS", "recording.uploaded", "analytics-recording",
            api.EventKindRecordingUploaded, p.flushRecording},
    }
    for _, c := range cfgs {
        c := c
        wg.Add(1)
        go func() {
            defer wg.Done()
            if err := p.runOne(ctx, c.stream, c.subject, c.durable, c.flushFn); err != nil {
                p.log.Error("ingest worker exited", "subj", c.subject, "err", err)
            }
        }()
    }
    wg.Wait()
    return ctx.Err()
}

type EnvWithMsg struct {
    Env api.EventEnvelope
    Msg jetstream.Msg
}

func (p *ingestImpl) runOne(
    ctx context.Context,
    stream, subject, durable string,
    flush func(context.Context, []*EnvWithMsg) error,
) error {
    s, err := p.js.Stream(ctx, stream)
    if err != nil {
        return fmt.Errorf("stream %s: %w", stream, err)
    }
    cons, err := s.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
        Durable:        durable,
        FilterSubject:  subject,
        AckPolicy:      jetstream.AckExplicitPolicy,
        DeliverPolicy:  jetstream.DeliverAllPolicy,
        MaxAckPending:  20_000,
        AckWait:        2 * time.Minute,
    })
    if err != nil {
        return fmt.Errorf("consumer %s: %w", durable, err)
    }
    iter, err := cons.Messages(jetstream.PullMaxMessages(maxBatchSize))
    if err != nil {
        return fmt.Errorf("messages iter: %w", err)
    }
    defer iter.Stop()

    batch := make([]*EnvWithMsg, 0, maxBatchSize)
    timer := time.NewTimer(maxBatchWindow)
    defer timer.Stop()

    flushNow := func() {
        if len(batch) == 0 {
            return
        }
        if err := p.flushWithRetry(ctx, batch, flush, subject); err != nil {
            p.log.Error("flush failed terminally", "subj", subject, "err", err)
        }
        batch = batch[:0]
        if !timer.Stop() {
            select { case <-timer.C: default: }
        }
        timer.Reset(maxBatchWindow)
    }

    for {
        select {
        case <-ctx.Done():
            flushNow()
            return ctx.Err()
        case <-timer.C:
            flushNow()
        default:
        }

        msg, err := iter.Next()
        if err != nil {
            if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
                flushNow()
                return ctx.Err()
            }
            p.log.Warn("iter.Next", "subj", subject, "err", err)
            time.Sleep(100 * time.Millisecond)
            continue
        }

        env, perr := decodeEnvelope(msg.Data())
        if perr != nil {
            p.recordStat(subject, func(s *api.SubjectStats) { s.DeadLetter++; s.LastError = perr.Error() })
            _ = p.publishDLQ(ctx, subject, msg, perr)
            _ = msg.Term()
            continue
        }

        if _, hit := p.seenIDs.Get(env.EventID); hit {
            _ = msg.Ack()
            continue
        }
        p.seenIDs.Add(env.EventID, struct{}{})

        batch = append(batch, &EnvWithMsg{Env: env, Msg: msg})
        p.recordStat(subject, func(s *api.SubjectStats) { s.Received++ })
        if len(batch) >= maxBatchSize {
            flushNow()
        }
    }
}

func (p *ingestImpl) flushWithRetry(
    ctx context.Context,
    batch []*EnvWithMsg,
    flush func(context.Context, []*EnvWithMsg) error,
    subject string,
) error {
    delay := minBackoff
    for attempt := 1; attempt <= maxAttempts; attempt++ {
        err := flush(ctx, batch)
        if err == nil {
            for _, b := range batch {
                _ = b.Msg.Ack()
            }
            p.recordStat(subject, func(s *api.SubjectStats) { s.Inserted += uint64(len(batch)) })
            return nil
        }
        if errors.Is(err, ErrInvalidPayload) {
            for _, b := range batch {
                _ = p.publishDLQ(ctx, subject, b.Msg, err)
                _ = b.Msg.Term()
            }
            p.recordStat(subject, func(s *api.SubjectStats) { s.DeadLetter += uint64(len(batch)); s.LastError = err.Error() })
            return err
        }
        p.log.Warn("flush retry", "subj", subject, "attempt", attempt, "err", err)
        if attempt == maxAttempts {
            for _, b := range batch {
                _ = b.Msg.NakWithDelay(maxBackoff)
            }
            p.recordStat(subject, func(s *api.SubjectStats) { s.Failed += uint64(len(batch)); s.LastError = err.Error() })
            return err
        }
        jitter := time.Duration(rand.Int64N(int64(delay) / 5))
        time.Sleep(delay + jitter - time.Duration(rand.Int64N(int64(delay)/5)))
        delay *= 4
        if delay > maxBackoff {
            delay = maxBackoff
        }
    }
    return nil
}

func (p *ingestImpl) flushCalls(ctx context.Context, batch []*EnvWithMsg) error {
    rows := make([]store.CallRow, 0, len(batch))
    for _, b := range batch {
        var pl store.CallPayload
        if err := json.Unmarshal(b.Env.Payload, &pl); err != nil {
            return fmt.Errorf("call payload: %w: %w", ErrInvalidPayload, err)
        }
        rows = append(rows, store.CallRow{
            Date:        b.Env.Timestamp.UTC(),
            Ts:          b.Env.Timestamp.UTC(),
            TenantID:    b.Env.TenantID,
            ProjectID:   pl.ProjectID,
            OperatorID:  pl.OperatorID,
            CallID:      pl.CallID,
            Status:      pl.Status,
            DurationSec: pl.DurationSec,
            HangupCause: pl.HangupCause,
            RegionCode:  pl.RegionCode,
            AttemptNo:   pl.AttemptNo,
            TrunkUsed:   pl.TrunkUsed,
            EventID:     b.Env.EventID,
        })
    }
    return p.ch.InsertCalls(ctx, rows)
}

func (p *ingestImpl) flushOperatorStates(ctx context.Context, batch []*EnvWithMsg) error {
    rows := make([]store.OperatorStateRow, 0, len(batch))
    for _, b := range batch {
        var pl store.OperatorStatePayload
        if err := json.Unmarshal(b.Env.Payload, &pl); err != nil {
            return fmt.Errorf("op-state payload: %w: %w", ErrInvalidPayload, err)
        }
        rows = append(rows, store.OperatorStateRow{
            Date:               b.Env.Timestamp.UTC(),
            Ts:                 b.Env.Timestamp.UTC(),
            TenantID:           b.Env.TenantID,
            UserID:             pl.UserID,
            State:              pl.State,
            DurationInStateSec: pl.DurationInStateSec,
            ProjectID:          pl.ProjectID,
            EventID:            b.Env.EventID,
        })
    }
    return p.ch.InsertOperatorStates(ctx, rows)
}

func (p *ingestImpl) flushRecording(ctx context.Context, batch []*EnvWithMsg) error {
    rows := make([]store.RecordingRow, 0, len(batch))
    for _, b := range batch {
        var pl store.RecordingPayload
        if err := json.Unmarshal(b.Env.Payload, &pl); err != nil {
            return fmt.Errorf("recording payload: %w: %w", ErrInvalidPayload, err)
        }
        rows = append(rows, store.RecordingRow{
            Date:                b.Env.Timestamp.UTC(),
            Ts:                  b.Env.Timestamp.UTC(),
            TenantID:            b.Env.TenantID,
            ProjectID:           pl.ProjectID,
            CallID:              pl.CallID,
            FsNode:              pl.FsNode,
            S3Key:               pl.S3Key,
            SizeBytes:           pl.SizeBytes,
            DurationSec:         pl.DurationSec,
            EncryptionKeyAlias:  pl.EncryptionKeyAlias,
            EventID:             b.Env.EventID,
        })
    }
    return p.ch.InsertRecordingUploads(ctx, rows)
}

func (p *ingestImpl) publishDLQ(ctx context.Context, subject string, msg jetstream.Msg, cause error) error {
    dlqSubject := "dlq.analytics." + subject
    headers := nats.Header{}
    headers.Set("Original-Subject", subject)
    headers.Set("Cause", cause.Error())
    if msg != nil {
        for k, v := range msg.Headers() {
            headers[k] = v
        }
    }
    var data []byte
    if msg != nil { data = msg.Data() }
    _, err := p.js.PublishMsg(ctx, &nats.Msg{
        Subject: dlqSubject,
        Header:  headers,
        Data:    data,
    })
    return err
}

func (p *ingestImpl) recordStat(subj string, mut func(*api.SubjectStats)) {
    p.statsMu.Lock()
    defer p.statsMu.Unlock()
    s := p.statsBySubj[subj]
    if s == nil {
        s = &api.SubjectStats{}
        p.statsBySubj[subj] = s
    }
    mut(s)
}

func (p *ingestImpl) Stats() api.IngestStats {
    p.statsMu.RLock()
    defer p.statsMu.RUnlock()
    out := api.IngestStats{PerSubject: make(map[string]api.SubjectStats, len(p.statsBySubj))}
    for k, v := range p.statsBySubj {
        out.PerSubject[k] = *v
    }
    return out
}

func decodeEnvelope(data []byte) (api.EventEnvelope, error) {
    var env api.EventEnvelope
    if err := json.Unmarshal(data, &env); err != nil {
        return env, fmt.Errorf("unmarshal envelope: %w: %w", ErrInvalidPayload, err)
    }
    if env.EventID == uuid.Nil || env.TenantID == uuid.Nil {
        return env, fmt.Errorf("missing required fields: %w", ErrInvalidPayload)
    }
    return env, nil
}
```

- [ ] `internal/analytics/store/ch.go` — implements `Sink` over `clickhouse-go/v2`:

```go
package store

import (
    "context"
    "fmt"
    "time"

    "github.com/ClickHouse/clickhouse-go/v2"
    chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"
    "github.com/google/uuid"
)

type CallRow struct {
    Date        time.Time
    Ts          time.Time
    TenantID    uuid.UUID
    ProjectID   uuid.UUID
    OperatorID  uuid.UUID
    CallID      uuid.UUID
    Status      string
    DurationSec uint32
    HangupCause string
    RegionCode  string
    AttemptNo   uint8
    TrunkUsed   string
    EventID     uuid.UUID
}

type OperatorStateRow struct {
    Date               time.Time
    Ts                 time.Time
    TenantID           uuid.UUID
    UserID             uuid.UUID
    State              string
    DurationInStateSec uint32
    ProjectID          *uuid.UUID
    EventID            uuid.UUID
}

type RecordingRow struct {
    Date               time.Time
    Ts                 time.Time
    TenantID           uuid.UUID
    ProjectID          uuid.UUID
    CallID             uuid.UUID
    FsNode             string
    S3Key              string
    SizeBytes          uint64
    DurationSec        uint32
    EncryptionKeyAlias string
    EventID            uuid.UUID
}

type CHSink struct {
    conn chdriver.Conn
}

func NewCHSink(conn chdriver.Conn) *CHSink { return &CHSink{conn: conn} }

func (s *CHSink) InsertCalls(ctx context.Context, rows []CallRow) error {
    if len(rows) == 0 { return nil }
    batch, err := s.conn.PrepareBatch(ctx, `
        insert into events_calls (
            date, ts, tenant_id, project_id, operator_id, call_id,
            status, duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id
        )`)
    if err != nil { return fmt.Errorf("prepare batch calls: %w", err) }
    for _, r := range rows {
        if err := batch.Append(
            r.Date, r.Ts, r.TenantID, r.ProjectID, r.OperatorID, r.CallID,
            r.Status, r.DurationSec, r.HangupCause, r.RegionCode, r.AttemptNo, r.TrunkUsed, r.EventID,
        ); err != nil {
            _ = batch.Abort()
            return fmt.Errorf("append calls row: %w", err)
        }
    }
    if err := batch.Send(); err != nil {
        return fmt.Errorf("send calls batch: %w: %w", ErrTransient, err)
    }
    return nil
}

func (s *CHSink) InsertOperatorStates(ctx context.Context, rows []OperatorStateRow) error { /* analogous */ return nil }
func (s *CHSink) InsertRecordingUploads(ctx context.Context, rows []RecordingRow) error  { /* analogous */ return nil }
```

The two further `Insert*` methods follow the same pattern; full code lives in the file but is omitted from the plan for brevity.

#### 3.4 — Wiring

- [ ] `cmd/worker/main.go` — adds an analytics block:

```go
chOpts, _ := clickhouse.ParseDSN(cfg.ClickHouse.DSN)
chConn, err := clickhouse.Open(chOpts)
if err != nil { return err }
sink := store.NewCHSink(chConn)
js, err := jetstream.New(natsConn)
if err != nil { return err }
pipe, err := analyticsService.NewIngest(js, sink, log)
if err != nil { return err }
errg.Go(func() error { return pipe.Run(ctx) })

// Prometheus collector for stats
prometheus.MustRegister(observability.NewIngestCollector(pipe.Stats))
```

- [ ] Stream/consumer config is created idempotently — `nats stream add --subjects 'dialer.call.finalized,operator.state.changed,recording.uploaded' --retention work-queue --max-age 7d --replicas 3` is part of the infra script in Plan 01; the worker only joins the stream.

#### 3.5 — Verification

- [ ] All tests in §3.2 pass.

- [ ] Manual smoke test: publish 1,000 envelopes through `nats pub`, observe `select count() from events_calls` returns 1,000 within 5s.

- [ ] Coverage on `internal/analytics/service/ingest*.go` ≥85%.

---

### Task 4: `MetricsQuery` — typed, tenant-scoped queries with Redis cache

The service-level API the FE will call. Queries are typed Go functions; the SQL lives in `store/queries/*.sql` and is loaded via `go:embed`. Redis caches read-heavy results with 30s/5min TTLs depending on freshness needs.

#### 4.1 — Public surface

- [ ] `internal/analytics/api/metrics.go`:

```go
package api

import (
    "context"
    "time"

    "github.com/google/uuid"
)

type Window struct {
    From time.Time
    To   time.Time
}

func (w Window) Validate() error {
    if w.From.IsZero() || w.To.IsZero() { return ErrInvalidWindow }
    if !w.From.Before(w.To) { return ErrInvalidWindow }
    if w.To.Sub(w.From) > 366*24*time.Hour { return ErrInvalidWindow }
    return nil
}

type CallsQuery struct {
    TenantID  uuid.UUID
    ProjectID *uuid.UUID
    Window    Window
}

type CallsResult struct {
    Total       uint64
    Successful  uint64
    Failed      uint64
    Refusals    uint64
    AvgDurSec   float64
    TotalDurSec uint64
    ByStatus    []StatusBucket
}

type StatusBucket struct {
    Status string
    Count  uint64
}

type OperatorStateQuery struct {
    TenantID   uuid.UUID
    OperatorID *uuid.UUID
    ProjectID  *uuid.UUID
    Window     Window
}

type OperatorStateBreakdown struct {
    TalkSec  uint64
    PauseSec uint64
    ReadySec uint64
    WrapSec  uint64
}

type RegionProgressQuery struct {
    TenantID  uuid.UUID
    ProjectID uuid.UUID
    Window    Window
}

type RegionProgressRow struct {
    RegionCode string
    Done       uint64
    Plan       uint64
    Progress   float64
}

type HourlyQuery struct {
    TenantID  uuid.UUID
    ProjectID *uuid.UUID
    Window    Window
}

type HourlyBucket struct {
    Hour      time.Time
    Count     uint64
    AvgDurSec float64
}

type OperatorComparisonsQuery struct {
    TenantID  uuid.UUID
    ProjectID uuid.UUID
    Window    Window
}

type OperatorComparisonRow struct {
    OperatorID    uuid.UUID
    DisplayName   string // resolved post-query from auth/users cache
    CallsTotal    uint64
    SuccessRate   float64
    AvgTalkSec    float64
    PauseShare    float64
    AboveTeamAvg  bool
}

type MetricsQuery interface {
    Calls(ctx context.Context, q CallsQuery) (CallsResult, error)
    OperatorState(ctx context.Context, q OperatorStateQuery) (OperatorStateBreakdown, error)
    RegionProgress(ctx context.Context, q RegionProgressQuery) ([]RegionProgressRow, error)
    Hourly(ctx context.Context, q HourlyQuery) ([]HourlyBucket, error)
    OperatorComparisons(ctx context.Context, q OperatorComparisonsQuery) ([]OperatorComparisonRow, error)
}
```

#### 4.2 — Failing tests

- [ ] `metrics_calls_test.go::TestCalls_TenantScoped_RejectsNilTenant` — `q.TenantID == uuid.Nil` returns `ErrTenantRequired`.

- [ ] `TestCalls_HappyPath_AggregatesByStatus` — fixture: 100 calls, 70 success / 20 fail / 10 refusal. Assert `Total=100`, `Successful=70`, `ByStatus` length=3.

- [ ] `TestCalls_RespectsProjectFilter` — fixture has two projects; query with `ProjectID=&proj2`; assert only proj2 rows counted.

- [ ] `TestOperatorState_AggregatesBucketsCorrectly` — operator has 3 sessions across 2 days; assert `TalkSec/PauseSec/ReadySec/WrapSec` match the hand-computed expected.

- [ ] `TestRegionProgress_JoinsWithPostgresQuotaPlan` — fixture has quota plan `MSK=1000, SPB=500` in Postgres `project_quotas`; CH has 700 success in MSK, 250 in SPB; asserts `Progress = {0.7, 0.5}`.

- [ ] `TestHourly_TimezoneAndBucketing` — verifies times are in UTC and bucketing matches `toStartOfHour`.

- [ ] `TestOperatorComparisons_FlagsAboveTeamAvg` — three operators with success-rates `0.8, 0.5, 0.3`; team avg 0.533; the first row `AboveTeamAvg=true`.

- [ ] `cache_test.go::TestCache_HitsRedisOnSecondCall` — fake Redis records `Get`/`Set`; second `Calls(...)` call within TTL hits cache.

- [ ] `TestCache_BypassesForLiveTail` — `Window.To > now-5min` ⇒ no `Set` call.

#### 4.3 — Implementation

- [ ] `internal/analytics/store/queries/calls.sql`:

```sql
select
    sumMerge(cnt)                                                          as total,
    sumMergeIf(cnt, status = 'success')                                    as successful,
    sumMergeIf(cnt, status = 'fail')                                       as failed,
    sumMergeIf(cnt, status = 'refusal')                                    as refusals,
    if(sumMerge(cnt) = 0, 0, sumMerge(duration_sec) / sumMerge(cnt))       as avg_dur_sec,
    sumMerge(duration_sec)                                                 as total_dur_sec
from mv_calls_hourly
where tenant_id = ?
  and ({{if .HasProject}} project_id = ? and {{end}} 1 = 1)
  and bucket_hour >= ?
  and bucket_hour <  ?;

-- second result set: by-status breakdown
select status, sumMerge(cnt) as cnt
from mv_calls_hourly
where tenant_id = ?
  and ({{if .HasProject}} project_id = ? and {{end}} 1 = 1)
  and bucket_hour >= ? and bucket_hour < ?
group by status
order by cnt desc;
```

- [ ] `internal/analytics/service/metrics_calls.go` (excerpt):

```go
package service

import (
    "context"
    "fmt"

    "github.com/google/uuid"
    "github.com/sociopulse/sociopulse/internal/analytics/api"
)

func (s *Service) Calls(ctx context.Context, q api.CallsQuery) (api.CallsResult, error) {
    if q.TenantID == uuid.Nil { return api.CallsResult{}, api.ErrTenantRequired }
    if err := q.Window.Validate(); err != nil { return api.CallsResult{}, err }

    cacheKey := s.cacheKey("calls", q)
    if r, ok, err := s.cache.GetCalls(ctx, cacheKey); err == nil && ok {
        return r, nil
    }

    sql, args := s.buildCallsSQL(q)
    var res api.CallsResult
    row := s.ch.QueryRow(ctx, sql.totals, args.totals...)
    if err := row.Scan(&res.Total, &res.Successful, &res.Failed, &res.Refusals, &res.AvgDurSec, &res.TotalDurSec); err != nil {
        return res, fmt.Errorf("calls totals: %w", err)
    }

    rows, err := s.ch.Query(ctx, sql.byStatus, args.byStatus...)
    if err != nil { return res, fmt.Errorf("calls by-status: %w", err) }
    defer rows.Close()
    for rows.Next() {
        var b api.StatusBucket
        if err := rows.Scan(&b.Status, &b.Count); err != nil {
            return res, fmt.Errorf("scan: %w", err)
        }
        res.ByStatus = append(res.ByStatus, b)
    }
    if err := rows.Err(); err != nil { return res, err }

    if !s.isLiveTail(q.Window) {
        _ = s.cache.SetCalls(ctx, cacheKey, res, 30*time.Second)
    }
    return res, nil
}

func (s *Service) cacheKey(kind string, q any) string {
    raw, _ := json.Marshal(q)
    sum := sha256.Sum256(raw)
    return fmt.Sprintf("analytics:%s:%s", kind, hex.EncodeToString(sum[:8]))
}

func (s *Service) isLiveTail(w api.Window) bool {
    return time.Since(w.To) < 5*time.Minute
}
```

- [ ] `internal/analytics/service/cache.go` — typed wrappers around `redis.Client`. JSON encoding for cached values; key prefix `analytics:`. The cache is best-effort: any Redis error logs at `warn` and falls through to the underlying query.

- [ ] `internal/analytics/store/queries/region_progress.sql`:

```sql
select
    region_code,
    sumMerge(success_cnt) as done
from mv_quotas_progress
where tenant_id = ?
  and project_id = ?
  and bucket_date >= toDate(?) and bucket_date < toDate(?)
group by region_code
order by region_code;
```

- The Postgres helper in `store/pg_quotas.go` reads the plan map (`region_code → planned_count`) for the same project from the `project_quotas` table introduced by Plan 06. The service joins them in Go (small set, ≤90 rows).

- [ ] `internal/analytics/store/queries/hourly.sql`:

```sql
select
    bucket_hour,
    sumMerge(cnt)                                                       as cnt,
    if(sumMerge(cnt) = 0, 0, sumMerge(duration_sec) / sumMerge(cnt))    as avg_dur_sec
from mv_calls_hourly
where tenant_id = ?
  and ({{if .HasProject}} project_id = ? and {{end}} 1 = 1)
  and bucket_hour >= ? and bucket_hour < ?
group by bucket_hour
order by bucket_hour;
```

- [ ] `internal/analytics/store/queries/operator_compare.sql`:

```sql
with team as (
    select
        if(sumMerge(calls_total) = 0, 0,
           sumMerge(calls_success) / sumMerge(calls_total)) as success_rate
    from mv_operator_kpi_daily
    where tenant_id = ? and project_id = ?
      and bucket_date >= toDate(?) and bucket_date < toDate(?)
)
select
    user_id,
    sumMerge(calls_total)                                              as calls_total,
    if(sumMerge(calls_total) = 0, 0,
       sumMerge(calls_success) / sumMerge(calls_total))                as success_rate,
    if(sumMerge(calls_total) = 0, 0,
       sumMerge(talk_sec) / sumMerge(calls_total))                     as avg_talk_sec,
    if(sumMerge(calls_total) + sumMerge(pause_sec) + sumMerge(ready_sec) = 0, 0,
       sumMerge(pause_sec) /
        (sumMerge(talk_sec) + sumMerge(pause_sec) + sumMerge(ready_sec) + sumMerge(wrap_sec))) as pause_share
from mv_operator_kpi_daily
where tenant_id = ? and project_id = ?
  and bucket_date >= toDate(?) and bucket_date < toDate(?)
group by user_id
order by calls_total desc
limit 200;
```

The `AboveTeamAvg` flag is computed in Go after the result lands.

- [ ] `internal/analytics/service/metrics_operator.go`, `metrics_region.go`, `metrics_hourly.go` — each follows the same `validate → cache lookup → CH query → cache write` shape. Region progress uses 5-minute TTL; comparisons use 5-minute TTL; hourly uses 30s TTL when the window includes "now-1h", else 5 minutes.

#### 4.4 — HTTP endpoints

- [ ] `internal/analytics/api/http.go`:

```go
package api

import (
    "net/http"

    "github.com/gin-gonic/gin"
)

type ServiceRO interface {
    MetricsQuery
    Overview(ctx context.Context, q OverviewQuery) (OverviewResult, error)
}

type OverviewQuery struct {
    TenantID  uuid.UUID
    ProjectID *uuid.UUID
    Window    Window
}

type OverviewResult struct {
    Calls          CallsResult            `json:"calls"`
    OperatorState  OperatorStateBreakdown `json:"operator_state"`
    RegionProgress []RegionProgressRow    `json:"region_progress"`
    Hourly         []HourlyBucket         `json:"hourly"`
}

func Endpoints(svc ServiceRO, requireAdmin gin.HandlerFunc) func(*gin.RouterGroup) {
    return func(r *gin.RouterGroup) {
        r.Use(requireAdmin)
        r.GET("/overview",   handleOverview(svc))
        r.GET("/calls",      handleCalls(svc))
        r.GET("/operators",  handleOperators(svc))
        r.GET("/regions",    handleRegions(svc))
        r.GET("/hourly",     handleHourly(svc))
    }
}

func handleOverview(svc ServiceRO) gin.HandlerFunc {
    return func(c *gin.Context) {
        q, err := decodeOverviewQuery(c)
        if err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
            return
        }
        result, err := svc.Overview(c.Request.Context(), q)
        if err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
            return
        }
        c.JSON(http.StatusOK, result)
    }
}
```

- [ ] Each `handle*` decodes the query from URL parameters (`from`, `to`, `project_id`), reads `tenant_id` from the request context (set by tenancy middleware in Plan 04), calls the service, and JSON-encodes the response via `c.JSON`. 400 on bad input; 500 on internal; 200 on success.

- [ ] `cmd/api/main.go` registers it under `/api/analytics` via `analyticsHTTP.Endpoints(svc, requireAdminMW)(router.Group("/api/analytics"))`.

#### 4.5 — Verification

- [ ] All tests in §4.2 pass. Coverage on `internal/analytics/service/metrics_*.go` ≥85%.

- [ ] Manual: `curl -H 'X-Tenant: <uuid>' '/api/analytics/calls?from=2026-05-01T00:00:00Z&to=2026-05-06T00:00:00Z'` returns valid JSON.

- [ ] Load test (in `tests/load/analytics_calls_test.go`, runs with `-tags=load`): 100 RPS for 1 min, p95 < 300 ms with cache warm.

---

---

## Self-review

**Spec coverage** (against §FR-I full, §6.4, §6.3 reports_jobs, §15.3, §17, §22):
- §6.4 ClickHouse таблицы: `events_calls`, `events_operator_state`, `events_recording_uploaded` — partition by toYYYYMM(date), ORDER BY (tenant_id, project_id, ts). Materialized views: `mv_calls_hourly`, `mv_operator_kpi_daily`, `mv_quotas_progress`. ✓
- IngestPipeline: NATS JetStream durable consumer group, in-memory batch (max 10 000 строк / 5 sec), CH INSERT через `clickhouse-go/v2`, retry с backoff, dead-letter NATS subject. Метрики `analytics_ingest_total`, `analytics_ingest_lag_seconds`. ✓
- MetricsQuery: типизированные запросы — Calls, OperatorStateBreakdown, RegionProgress, HourlyDistribution, OperatorComparisons. Tenant-фильтрация ВСЕГДА в WHERE. Redis-cache 30s/5min. ✓
- §FR-I1 6 преднастроенных отчётов в `internal/reports/templates/<kind>/{xlsx.go,csv.go,pdf.go}`: efficiency_operators, project_summary, calls_by_status, financial, quality_control, hourly_activity. ✓
- §FR-I2 произвольный отчёт `POST /api/reports/custom` → 202 + jobID для async. ✓
- §FR-I3 async через asynq для period > 30 дней OR rows > 100 000; persist в S3 (`reports` bucket), 24h presigned URL. ✓
- HTTP endpoints: GET `/api/reports`, GET `/api/reports/:kind`, POST `/api/reports/:kind/export`, POST `/api/reports/custom`, GET `/api/reports/jobs/:jobID`, GET `/api/reports/jobs/:jobID/download`. Handlers return `gin.HandlerFunc`; path params read via `c.Param("kind")` / `c.Param("jobID")`. ✓
- §15.3 метрики: `sociopulse_reports_jobs_total{kind,status}`, `sociopulse_reports_render_duration_seconds`, `sociopulse_analytics_query_duration_seconds`. ✓
- §17 тестовая стратегия: unit (renderer на минимальных данных + структура XLSX через excelize-read, CSV через csv.Reader, PDF через page-count), integration end-to-end async-job, load-test (100 RPS for 1 min, p95 < 300ms с прогретым кешем). Coverage ≥ 80%. ✓
- §22 прототип `admin-pages-2.jsx::AdminReports` — UI потребляет эти endpoints, реализуется в Plan 19. ✓
- Audit-log: все экспорты в audit_log с параметрами, без ПДн в payload. ✓

**Placeholder scan:** none. Каждый из 6 templates имеет реализацию с реальным excelize/gopdf-кодом.

**Type/name consistency:** `IngestPipeline`, `MetricsQuery`, `ReportRenderer`, `JobQueue`, `JobConsumer` — стабильные имена. `internal/reports/api/` строго не зависит от ClickHouse-driver — потребляет только `analytics/api`.

**Out of scope (correctly deferred):**
- UI отчётов — Plan 19.
- Финансовые расчёты per-call — Plan 14.
- Real-time дашборды (live-charts) — backlog (CH не для этого).

Plan 13 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-13-analytics-reports.md`.**


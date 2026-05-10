# Analytics — Materialised View Read Pattern

> Plan 13.1 ships three AggregatingMergeTree state tables fed by
> materialised views over `events_calls` and `events_operator_state`.
> This doc tells you how to READ them. Plan 13.2 metric queries
> MUST follow the patterns here — direct SELECT on aggregate-
> function columns returns garbage.

## TL;DR

- Use the `mv_*` views by name; never read the `mv_*_state` table directly,
  and never read an individual feeder MV (`mv_operator_kpi_daily_calls`,
  `mv_operator_kpi_daily_states`) — those are write-side only.
- Wrap every aggregate column in `*Merge`: `sumMerge(cnt)`, `uniqMerge(distinct_calls)`.
- Always include `tenant_id = ?` as the FIRST predicate.
- Use `OPTIMIZE TABLE mv_*_state FINAL` only in tests; production reads accept
  eventual rollup convergence.
- When the access pattern requires lookups outside the MV's ORDER BY, fall back
  to the source table (`events_calls` / `events_operator_state`) and pay the
  scan cost knowingly.

## The three MVs

| Read endpoint | State table | Source(s) | ORDER BY | Aggregates |
|---|---|---|---|---|
| `mv_calls_hourly` (MV `TO …_state`) | `mv_calls_hourly_state` | `events_calls` | `(tenant_id, project_id, bucket_hour, status, region_code)` | `sumState(cnt)`, `sumState(duration_sec)`, `uniqState(distinct_calls)` |
| `mv_operator_kpi_daily` (plain VIEW over state) | `mv_operator_kpi_daily_state` | `events_calls` + `events_operator_state` (via `mv_operator_kpi_daily_calls` and `mv_operator_kpi_daily_states` feeders) | `(tenant_id, user_id, project_id, bucket_date)` | `sumState(calls_total/success/refusal/talk_sec/pause_sec/ready_sec/wrap_sec)` |
| `mv_quotas_progress` (MV `TO …_state`) | `mv_quotas_progress_state` | `events_calls` | `(tenant_id, project_id, region_code, bucket_date)` | `sumState(success_cnt/fail_cnt/refusal_cnt/other_cnt)` |

For the operator-KPI table, the canonical read endpoint is the plain
`CREATE VIEW mv_operator_kpi_daily AS SELECT * FROM mv_operator_kpi_daily_state`.
Read through that name, NOT the `_state` table or one of the two feeders.

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
- Putting `;` inside `--` SQL comments in multi-statement migrations —
  golang-migrate's `x-multi-statement` splitter cuts on `;` regardless of
  comment context. Fragmenting a `CREATE MATERIALIZED VIEW` mid-comment
  yields a parse-time CH error. (Hit during Plan 13.1 Task 3; commit
  `6247ad1` removed the offending semicolon.)

## Two-feeder MV pattern (operator KPI)

`mv_operator_kpi_daily_state` is fed by TWO materialised views writing to
the same shared state table:

- `mv_operator_kpi_daily_calls` reads `events_calls`, fills the call-count
  columns (`calls_total`, `calls_success`, `calls_refusal`), zeros the
  duration columns (`talk_sec`, `pause_sec`, `ready_sec`, `wrap_sec`).
- `mv_operator_kpi_daily_states` reads `events_operator_state`, fills the
  duration columns, zeros the call-count columns.

`AggregatingMergeTree` merges them on the
`(tenant_id, user_id, project_id, bucket_date)` key. The
`mv_operator_kpi_daily` plain VIEW exposes the merged state table as the
canonical read endpoint — consumers select from it and apply `*Merge`
finals; they MUST NOT read from `_state` or either feeder directly.

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

- `migrations/clickhouse/000004_mv_calls_hourly.up.sql`,
  `migrations/clickhouse/000005_mv_operator_kpi_daily.up.sql`,
  `migrations/clickhouse/000006_mv_quotas_progress.up.sql` — the canonical
  MV definitions.
- `cmd/migrator/integration_ch_test.go` — `TestMV_CallsHourly_RollupShape`,
  `TestMV_OperatorKpiDaily_AggregatesStatesAndCalls`,
  `TestMV_QuotasProgress_RegionGroupedByDay`,
  `TestMV_CallsHourly_RawVsMVParity` — rollup-shape tests with fixture +
  assertion examples.
- `docs/references/plan-13-analytics.md` — gotchas (sumMerge, multi-statement,
  LowCardinality cap).
- Master spec §6.4 — original schema spec.
- Plan 13.2 (TBD) — `internal/analytics/store/queries/*.sql` will be the
  first real consumer of these read patterns.

-- Plan 13.2.5 Task 4 — recreate the four MATERIALIZED VIEW
-- definitions whose source tables were renamed in migrations
-- 000007/008/009. The MV state tables (mv_calls_hourly_state,
-- mv_operator_kpi_daily_state, mv_quotas_progress_state) were NOT
-- dropped — only the MV write-side definitions, which were dropped in
-- 000007/008. We recreate them here against the new ReplacingMergeTree
-- source tables. The SELECT bodies are byte-identical to the original
-- definitions in 000004/5/6 — the only change is that the source
-- tables now have a ReplacingMergeTree engine and ORDER BY
-- (tenant_id, event_id). The MV runs on INSERT so storage-engine
-- choice is transparent here.

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
GROUP BY tenant_id, project_id, bucket_hour, status, region_code;

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
GROUP BY tenant_id, user_id, project_id, bucket_date;

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

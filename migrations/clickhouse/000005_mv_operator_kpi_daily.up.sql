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
    -- project_id is Nullable in events_operator_state because state
    -- changes can happen outside any project (e.g. login/logout). We
    -- coalesce to a sentinel zero-UUID so the AggregatingMergeTree
    -- ORDER BY tuple (which forbids nulls) collapses these into one
    -- bucket. Note: the migrator splits by  literal semicolons
    -- including inside SQL comments, so this comment uses none.
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

CREATE VIEW IF NOT EXISTS mv_operator_kpi_daily AS
SELECT * FROM mv_operator_kpi_daily_state

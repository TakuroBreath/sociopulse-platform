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

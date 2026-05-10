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

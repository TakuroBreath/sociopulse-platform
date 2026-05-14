-- Reverse the ReplacingMergeTree migration: rename current table aside,
-- recreate the legacy MergeTree schema, copy data back (dropping the
-- _inserted_at DateTime64(3) and re-using the legacy DateTime default),
-- drop the temporary, and drop the dependent MVs (they will be
-- recreated by 000004/5/6 .up.sql via migration 000010's .down.sql).

DROP VIEW IF EXISTS mv_calls_hourly;
DROP VIEW IF EXISTS mv_operator_kpi_daily_calls;
DROP VIEW IF EXISTS mv_quotas_progress;

RENAME TABLE events_calls TO events_calls_new;

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
SETTINGS index_granularity = 8192;

INSERT INTO events_calls
SELECT
    date,
    ts,
    tenant_id,
    project_id,
    operator_id,
    call_id,
    status,
    duration_sec,
    hangup_cause,
    region_code,
    attempt_no,
    trunk_used,
    event_id,
    toDateTime(_inserted_at) AS _inserted_at
FROM events_calls_new;

DROP TABLE IF EXISTS events_calls_new

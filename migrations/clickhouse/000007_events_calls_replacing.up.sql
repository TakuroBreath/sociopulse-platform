-- Plan 13.2.5 Task 4 — flip events_calls engine to ReplacingMergeTree
-- so duplicate event_ids that slip past the consumer-side DedupLRU are
-- reconciled at storage layer. ClickHouse does not support
-- ALTER TABLE … MODIFY ENGINE, so this migration uses the canonical
-- rename-old + create-new + INSERT … SELECT + drop-old pattern.
--
-- ORDER BY trade-off: legacy was (tenant_id, project_id, ts) for
-- per-project hourly scan performance. New is (tenant_id, event_id)
-- because ReplacingMergeTree dedups by ORDER BY tuple — without the
-- event_id leading the tuple, two rows with the same event_id but
-- different timestamps would not collapse. The aggregating MVs in
-- migrations 000004/5/6 read via sumMerge over their own *_state
-- tables (ORDER BY (tenant_id, project_id, bucket_hour, …)) so the
-- source-table ORDER BY change does not affect dashboard queries.
-- Direct source-table scans by project_id (debug only) trade speed
-- for dedup correctness — acceptable per Plan 13.2.5 § Task 4.
--
-- MV dependency: the MVs CREATE MATERIALIZED VIEW TO state-table AS
-- SELECT FROM events_calls reference the table by name -- the rename
-- breaks them. We drop the MV definition (the VIEW), keep the state
-- table (no data loss), and recreate the MV in migration 000010
-- against the new source table. The MV state table itself
-- (mv_calls_hourly_state) is untouched.

DROP VIEW IF EXISTS mv_calls_hourly;
DROP VIEW IF EXISTS mv_operator_kpi_daily_calls;
DROP VIEW IF EXISTS mv_quotas_progress;

RENAME TABLE events_calls TO events_calls_legacy;

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
    _inserted_at  DateTime64(3) DEFAULT now64()
)
ENGINE = ReplacingMergeTree(_inserted_at)
PARTITION BY toYYYYMM(date)
ORDER BY (tenant_id, event_id)
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
    toDateTime64(_inserted_at, 3) AS _inserted_at
FROM events_calls_legacy;

DROP TABLE IF EXISTS events_calls_legacy

-- Plan 13.2.5 Task 4 — same rename-and-recreate pattern for
-- events_operator_state. See migration 000007 header for ORDER BY
-- trade-off rationale and MV-dependency strategy.
--
-- mv_operator_kpi_daily_states (the operator-side feeder) references
-- this table and must be dropped before the rename -- it will be
-- recreated in 000010.

DROP VIEW IF EXISTS mv_operator_kpi_daily_states;

RENAME TABLE events_operator_state TO events_operator_state_legacy;

CREATE TABLE IF NOT EXISTS events_operator_state
(
    date                   Date,
    ts                     DateTime64(3),
    tenant_id              UUID,
    user_id                UUID,
    state                  LowCardinality(String),
    duration_in_state_sec  UInt32,
    project_id             Nullable(UUID),
    event_id               UUID,
    _inserted_at           DateTime64(3) DEFAULT now64()
)
ENGINE = ReplacingMergeTree(_inserted_at)
PARTITION BY toYYYYMM(date)
ORDER BY (tenant_id, event_id)
TTL date + INTERVAL 26 MONTH
SETTINGS index_granularity = 8192;

INSERT INTO events_operator_state
SELECT
    date,
    ts,
    tenant_id,
    user_id,
    state,
    duration_in_state_sec,
    project_id,
    event_id,
    toDateTime64(_inserted_at, 3) AS _inserted_at
FROM events_operator_state_legacy;

DROP TABLE IF EXISTS events_operator_state_legacy

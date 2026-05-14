-- Reverse: drop the dependent MV, rename current aside, recreate
-- legacy schema, copy data back, drop temporary.

DROP VIEW IF EXISTS mv_operator_kpi_daily_states;

RENAME TABLE events_operator_state TO events_operator_state_new;

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
    _inserted_at           DateTime DEFAULT now()
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(date)
ORDER BY (tenant_id, user_id, ts)
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
    toDateTime(_inserted_at) AS _inserted_at
FROM events_operator_state_new;

DROP TABLE IF EXISTS events_operator_state_new

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
SETTINGS index_granularity = 8192

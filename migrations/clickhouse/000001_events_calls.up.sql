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
SETTINGS index_granularity = 8192

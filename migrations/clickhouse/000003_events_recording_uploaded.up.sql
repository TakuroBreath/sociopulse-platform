CREATE TABLE IF NOT EXISTS events_recording_uploaded
(
    date                  Date,
    ts                    DateTime64(3),
    tenant_id             UUID,
    project_id            UUID,
    call_id               UUID,
    fs_node               LowCardinality(String),
    s3_key                String,
    size_bytes            UInt64,
    duration_sec          UInt32,
    encryption_key_alias  LowCardinality(String),
    event_id              UUID,
    _inserted_at          DateTime DEFAULT now()
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(date)
ORDER BY (tenant_id, ts)
TTL date + INTERVAL 26 MONTH
SETTINGS index_granularity = 8192

-- Plan 13.2.5 Task 4 — same rename-and-recreate pattern for
-- events_recording_uploaded. No MVs depend on this table in v1, so the
-- migration is a simple table swap.

RENAME TABLE events_recording_uploaded TO events_recording_uploaded_legacy;

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
    _inserted_at          DateTime64(3) DEFAULT now64()
)
ENGINE = ReplacingMergeTree(_inserted_at)
PARTITION BY toYYYYMM(date)
ORDER BY (tenant_id, event_id)
TTL date + INTERVAL 26 MONTH
SETTINGS index_granularity = 8192;

INSERT INTO events_recording_uploaded
SELECT
    date,
    ts,
    tenant_id,
    project_id,
    call_id,
    fs_node,
    s3_key,
    size_bytes,
    duration_sec,
    encryption_key_alias,
    event_id,
    toDateTime64(_inserted_at, 3) AS _inserted_at
FROM events_recording_uploaded_legacy;

DROP TABLE IF EXISTS events_recording_uploaded_legacy

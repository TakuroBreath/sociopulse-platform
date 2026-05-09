-- 000010_recording_evolve.down.sql
--
-- Reverse 000010_recording_evolve.up.sql. Restores the Plan 03 (000001_init)
-- shape of call_recordings on an EMPTY table. If the table has any rows the
-- migration refuses to run — silently discarding the new columns would destroy
-- data that has no equivalent in the legacy schema (bytes_size, integrity_ok,
-- ingest_agent_id, etc.).

begin;

-- ─── data-loss guard ─────────────────────────────────────────────────────────
do $$
begin
    if exists (select 1 from call_recordings limit 1) then
        raise exception
            'down migration would lose data — call_recordings has rows. '
            'Manually drop or migrate the data first before rolling back.';
    end if;
end$$;

-- ─── drop new constraints ─────────────────────────────────────────────────────
alter table call_recordings drop constraint if exists call_recordings_status_check;
alter table call_recordings drop constraint if exists call_recordings_call_id_unique;

-- ─── drop new indexes ─────────────────────────────────────────────────────────
drop index if exists call_recordings_search_idx;
drop index if exists call_recordings_status_delete_at_idx;
drop index if exists call_recordings_status_cold_at_idx;

-- ─── drop new PK, restore old PK on call_id ──────────────────────────────────
-- Remove the surrogate PK before dropping the id column.
do $$
begin
    if exists (
        select 1 from pg_constraint
        where conrelid = 'call_recordings'::regclass
          and contype = 'p'
          and conname = 'call_recordings_pkey'
    ) then
        alter table call_recordings drop constraint call_recordings_pkey;
    end if;
end$$;

alter table call_recordings add primary key (call_id);

-- ─── restore legacy columns with safe defaults ────────────────────────────────
alter table call_recordings
    add column if not exists created_at      timestamptz not null default now(),
    add column if not exists duration_sec    integer     not null default 0,
    add column if not exists retention_until date        not null
        default (now()::date + interval '365 days');

-- ─── rename audio_object_key → s3_key, sha256_hex → sha256 ───────────────────
do $$
begin
    if exists (
        select 1 from information_schema.columns
        where table_name = 'call_recordings' and column_name = 'audio_object_key'
    ) then
        alter table call_recordings rename column audio_object_key to s3_key;
    end if;
end$$;

do $$
begin
    if exists (
        select 1 from information_schema.columns
        where table_name = 'call_recordings' and column_name = 'sha256_hex'
    ) then
        alter table call_recordings rename column sha256_hex to sha256;
    end if;
end$$;

-- ─── revert delete_at from timestamptz back to date ──────────────────────────
alter table call_recordings
    alter column delete_at type date using delete_at::date;

-- ─── drop all new columns added in up ────────────────────────────────────────
alter table call_recordings
    drop column if exists id,
    drop column if exists dek_object_key,
    drop column if exists bytes_size,
    drop column if exists duration_ms,
    drop column if exists sample_rate,
    drop column if exists status,
    drop column if exists committed_at,
    drop column if exists cold_at,
    drop column if exists recorded_at,
    drop column if exists verified_at,
    drop column if exists integrity_ok,
    drop column if exists ingest_agent_id;

-- ─── restore legacy retention index ──────────────────────────────────────────
create index if not exists call_recordings_retention_idx
    on call_recordings (retention_until)
    where delete_at is null;

commit;

-- 000010_recording_evolve.up.sql
--
-- Plan 12.1 Task 1 — evolve call_recordings from the Plan 03 shape to the
-- Plan 12 shape. The table is empty in production at the time this migration
-- runs (Plan 12 has not shipped yet), so every ALTER is a no-op in terms of
-- data volume, but the migration is written to be data-preserving regardless:
-- backfills fire before NOT NULL constraints and the PK swap.
--
-- Changes summarised:
--   ADD columns: id, dek_object_key, bytes_size, duration_ms, sample_rate,
--                status, committed_at, cold_at, recorded_at, verified_at,
--                integrity_ok, ingest_agent_id.
--   RENAME: s3_key → audio_object_key, sha256 → sha256_hex.
--   TYPE CHANGE: delete_at date → timestamptz.
--   DROP columns: retention_until, duration_sec, created_at.
--   PK swap: call_id → id; call_id becomes UNIQUE.
--   INDEX: drop retention_idx; add status_cold_at, status_delete_at, search.
--   CHECK: status IN ('stored','cold','deleted').

begin;

-- ─── step 1: add new columns (nullable during transition) ─────────────────────
alter table call_recordings
    add column if not exists id              uuid,
    add column if not exists dek_object_key  text,
    add column if not exists bytes_size      bigint,
    add column if not exists duration_ms     bigint,
    add column if not exists sample_rate     integer     not null default 48000,
    add column if not exists status          text        not null default 'stored',
    add column if not exists committed_at    timestamptz,
    add column if not exists cold_at         timestamptz,
    add column if not exists recorded_at     timestamptz,
    add column if not exists verified_at     timestamptz,
    add column if not exists integrity_ok    boolean,
    add column if not exists ingest_agent_id text;

-- ─── step 2: backfill ─────────────────────────────────────────────────────────
-- The table is empty in production (Plan 12 has not shipped yet), so this
-- UPDATE is normally a no-op — but it stays safe even if a future restore
-- introduces rows: the whole migration runs in one transaction (begin/commit
-- at top/bottom), so a crash rolls back atomically and a re-run starts from
-- the pre-migration state. The `where id is null` predicate is defence-in-
-- depth: it avoids re-rolling new UUIDs over rows that some future hand-
-- patched runner might have already partially backfilled.
--
-- Backfill mapping:
--   id              — new surrogate PK; must be non-null before we can add a PK.
--   committed_at    — semantically "when the record was committed"; map from created_at.
--   duration_ms     — millisecond equivalent of duration_sec.
--   bytes_size      — no legacy value; use 0 as safe sentinel.
--   cold_at         — replaces retention_until; promote date → timestamptz at midnight UTC,
--                     anchored explicitly via timezone('UTC', …) to avoid session-tz drift.
--   recorded_at     — best approximation from created_at (no better source in legacy rows).
--   ingest_agent_id — no legacy value; use '' as safe sentinel.
update call_recordings set
    id              = gen_random_uuid(),
    committed_at    = created_at,
    duration_ms     = duration_sec::bigint * 1000,
    bytes_size      = 0,
    cold_at         = timezone('UTC', retention_until::timestamp),
    recorded_at     = created_at,
    ingest_agent_id = ''
where id is null;

-- ─── step 3: rename s3_key → audio_object_key, sha256 → sha256_hex ────────────
do $$
begin
    if exists (
        select 1 from information_schema.columns
        where table_name = 'call_recordings' and column_name = 's3_key'
    ) then
        alter table call_recordings rename column s3_key to audio_object_key;
    end if;
end$$;

do $$
begin
    if exists (
        select 1 from information_schema.columns
        where table_name = 'call_recordings' and column_name = 'sha256'
    ) then
        alter table call_recordings rename column sha256 to sha256_hex;
    end if;
end$$;

-- ─── step 4: change delete_at from date to timestamptz ────────────────────────
-- Anchor existing date values to midnight UTC explicitly. A direct
-- `date → timestamptz` cast resolves midnight in the *session* time zone, not
-- UTC, so on a non-UTC server we would silently shift the boundary by the
-- session offset. timezone('UTC', date::timestamp) pins the result to
-- 00:00:00+00 regardless of the runner's TimeZone setting.
alter table call_recordings
    alter column delete_at type timestamptz
        using timezone('UTC', delete_at::timestamp);

-- ─── step 5: drop columns replaced by the new ones ───────────────────────────
alter table call_recordings
    drop column if exists retention_until,
    drop column if exists duration_sec,
    drop column if exists created_at;

-- ─── step 6: enforce NOT NULL on all columns that must not be null ────────────
alter table call_recordings
    alter column id              set not null,
    alter column bytes_size      set not null,
    alter column duration_ms     set not null,
    alter column committed_at    set not null,
    alter column cold_at         set not null,
    alter column recorded_at     set not null,
    alter column ingest_agent_id set not null;

-- delete_at remains nullable (null = no scheduled deletion).

-- ─── step 7: PK swap — drop old PK on call_id, add new PK on id ──────────────
-- The legacy table was created with `call_id uuid primary key`, so Postgres
-- gave the constraint the default name `call_recordings_pkey`. The DO-block
-- guard makes this script idempotent on a *full re-run of an already-applied
-- migration* (e.g. `migrate up` after `migrate down`). Partial-step replay is
-- impossible inside a transaction — the wrapping begin/commit makes the whole
-- script atomic, so a crash rolls everything back.
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

alter table call_recordings add primary key (id);

-- call_id keeps its NOT NULL + FK from the original table; uniqueness is
-- enforced here for Plan 12.4 idempotency (re-ingest of the same call is a
-- conflict, not a duplicate insert).
alter table call_recordings
    drop constraint if exists call_recordings_call_id_unique;
alter table call_recordings
    add constraint call_recordings_call_id_unique unique (call_id);

-- ─── step 8: index housekeeping ───────────────────────────────────────────────
drop index if exists call_recordings_retention_idx;

-- Lifecycle sweeper: find stored recordings whose cold_at has passed.
create index if not exists call_recordings_status_cold_at_idx
    on call_recordings (status, cold_at)
    where status = 'stored';

-- Deletion sweeper: find stored/cold recordings with a scheduled delete_at.
create index if not exists call_recordings_status_delete_at_idx
    on call_recordings (status, delete_at)
    where status in ('stored', 'cold');

-- Cursor-pagination search (Plan 12.3): tenant × time DESC, tie-break by id.
create index if not exists call_recordings_search_idx
    on call_recordings (tenant_id, committed_at desc, id desc);

-- ─── step 9: add CHECK constraint for status ─────────────────────────────────
alter table call_recordings
    drop constraint if exists call_recordings_status_check;
alter table call_recordings
    add constraint call_recordings_status_check
    check (status in ('stored', 'cold', 'deleted'));

commit;

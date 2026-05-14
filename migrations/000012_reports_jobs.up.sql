-- 000012_reports_jobs.up.sql
--
-- Plan 13.3 Task 2 — evolve reports_jobs from the Plan 03 shape
-- (000001_init) to the Plan 13.3 shape.
--
-- The table is empty in production at the time this migration runs
-- (Plan 13.3 ships the first persisted reports flow), so every ALTER
-- is a no-op in terms of data volume, but the migration is written to
-- be safely re-runnable: each ALTER uses IF EXISTS / IF NOT EXISTS
-- and the whole script runs inside one BEGIN/COMMIT so a crash rolls
-- everything back atomically.
--
-- Changes summarised:
--
--   PK swap:          id uuid → id text (asynq task ids are text/ULID-ish).
--   ADD columns:      format, window_from, window_to, state, started_at,
--                     bytes_size, filename, download_url, created_by,
--                     notify_user_id.
--   DROP columns:     status (replaced by state), result_s3_key (replaced
--                     by download_url, which is the presigned URL),
--                     requested_by (replaced by created_by — same idea, no
--                     FK because the user might be deleted before the
--                     job's retention window expires; we keep the audit
--                     trail).
--   INDEX:            drop reports_jobs_queue_idx; add
--                     reports_jobs_tenant_created_idx (dashboard list
--                     view) and reports_jobs_tenant_state_idx (asynq
--                     consumer's filter scans).
--   CHECK:            state IN (5 lifecycle values), kind IN (7 report
--                     kinds), format IN ('xlsx','csv','pdf'),
--                     window_to > window_from.
--   GRANT:            tenancy_admin SELECT/INSERT/UPDATE — required by
--                     the asynq consumer in Plan 13.3 Task 6, which runs
--                     under BypassRLS for cross-tenant resolver lookups
--                     and then re-enters WithTenant for state transitions.
--                     The convention (000009/000011) is to grant only what
--                     the new cross-tenant consumer needs; no DELETE
--                     because terminal states are flags, not row deletion.

begin;

-- ─── step -1: empty-table guard ──────────────────────────────────────────────
-- The migration's column reshape (PK swap uuid→text, status→state CHECK,
-- kind→7-value enum CHECK) assumes the legacy table is empty. If a future
-- code path seeds reports_jobs via a side channel before this migration
-- runs, fail loudly rather than silently corrupting data — the legacy
-- 'kind'/'status' values would not satisfy the new CHECK constraints and
-- 'id' values would be overwritten with synthetic 'legacy-<uuid>' strings.
do $$
begin
    if (select count(*) from reports_jobs) > 0 then
        raise exception 'reports_jobs is non-empty (% rows); Plan 13.3 migration assumes empty table — manual mapping of legacy kind/status to new enums is required before re-running',
            (select count(*) from reports_jobs);
    end if;
end$$;

-- ─── step 0: drop the legacy index that referenced status before we drop the column ──
drop index if exists reports_jobs_queue_idx;

-- ─── step 1: drop the legacy CHECK constraint on status before dropping the column ──
do $$
declare
    cn text;
begin
    for cn in
        select conname from pg_constraint
        where conrelid = 'reports_jobs'::regclass and contype = 'c'
    loop
        execute format('alter table reports_jobs drop constraint %I', cn);
    end loop;
end$$;

-- ─── step 2: drop legacy columns we are not carrying forward ─────────────────
-- requested_by carried an FK on users(id); dropping it removes that FK.
-- The new created_by column is plain uuid (no FK) so the row survives a
-- user-row deletion (we keep the audit trail).
alter table reports_jobs
    drop column if exists status,
    drop column if exists result_s3_key,
    drop column if exists requested_by;

-- ─── step 3: PK swap — id uuid → id text ──────────────────────────────────────
-- The legacy table created id as uuid PRIMARY KEY with default
-- gen_random_uuid(). asynq emits text task ids, so we change the column
-- type. The table is empty so we drop+re-add the column rather than
-- attempting a USING cast.
alter table reports_jobs
    drop constraint if exists reports_jobs_pkey;
alter table reports_jobs
    drop column if exists id;
alter table reports_jobs
    add column id text;
update reports_jobs set id = 'legacy-' || gen_random_uuid()::text where id is null;
alter table reports_jobs
    alter column id set not null,
    add primary key (id);

-- ─── step 4: add new lifecycle columns ────────────────────────────────────────
alter table reports_jobs
    add column if not exists format         text        not null default 'xlsx',
    add column if not exists window_from    timestamptz,
    add column if not exists window_to      timestamptz,
    add column if not exists state          text        not null default 'queued',
    add column if not exists started_at     timestamptz,
    add column if not exists bytes_size     bigint      not null default 0,
    add column if not exists filename       text        not null default '',
    add column if not exists download_url   text        not null default '',
    add column if not exists created_by     uuid,
    add column if not exists notify_user_id uuid;

-- ─── step 5: backfill window_from/window_to defaults ──────────────────────────
-- The table is empty in production at the time this migration runs, so the
-- UPDATE is a no-op — but a future restore that introduces rows must not
-- violate the upcoming NOT NULL + CHECK constraints. Pin window_from/to to
-- a 1-second window centred on created_at; the CHECK (window_to > window_from)
-- holds and the row is recognisably synthetic.
update reports_jobs set
    window_from = created_at,
    window_to   = created_at + interval '1 second'
where window_from is null or window_to is null;
update reports_jobs set
    created_by     = '00000000-0000-0000-0000-000000000000',
    notify_user_id = '00000000-0000-0000-0000-000000000000'
where created_by is null or notify_user_id is null;

-- ─── step 6: enforce NOT NULL on the new columns ──────────────────────────────
alter table reports_jobs
    alter column window_from    set not null,
    alter column window_to      set not null,
    alter column created_by     set not null,
    alter column notify_user_id set not null;

-- params kept its NOT NULL default; finished_at remains nullable. error
-- + filename + download_url get the empty-string default so the Job DTO
-- can scan into a plain `string` without nil-pointer ceremony.
alter table reports_jobs
    alter column error set default '',
    alter column error set not null;
update reports_jobs set error = '' where error is null;

-- ─── step 7: state-machine + enum CHECK constraints ───────────────────────────
alter table reports_jobs
    add constraint reports_jobs_state_valid
        check (state in ('queued', 'running', 'succeeded', 'failed', 'canceled'));
alter table reports_jobs
    add constraint reports_jobs_kind_valid
        check (kind in ('operator_efficiency', 'project_summary', 'calls_by_status',
                        'finance', 'quality_control', 'hourly_activity', 'custom'));
alter table reports_jobs
    add constraint reports_jobs_format_valid
        check (format in ('xlsx', 'csv', 'pdf'));
alter table reports_jobs
    add constraint reports_jobs_window_ordered
        check (window_to > window_from);

-- ─── step 8: indexes for the dashboard + consumer access patterns ─────────────
create index if not exists reports_jobs_tenant_created_idx
    on reports_jobs (tenant_id, created_at desc);
create index if not exists reports_jobs_tenant_state_idx
    on reports_jobs (tenant_id, state);

-- RLS policy was created by 000001_init (reports_jobs_iso) and stays in
-- place — its (tenant_id = app.tenant_id) predicate works unchanged on
-- the new schema.

-- ─── step 9: tenancy_admin grants — Plan 13.3 Task 6 (asynq consumer) ─────────
grant select, insert, update on reports_jobs to tenancy_admin;

commit;

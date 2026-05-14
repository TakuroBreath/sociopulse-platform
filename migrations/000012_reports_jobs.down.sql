-- 000012_reports_jobs.down.sql
--
-- Reverse 000012_reports_jobs.up.sql — restore the Plan 03
-- (000001_init) shape of the reports_jobs table. The table itself
-- continues to exist (it was created by 000001_init) — we only roll
-- back the Plan 13.3 evolution.
--
-- Empty-table assumption (Plan 13.3 is the first persistence layer for
-- this table) lets the column type swap go via drop+re-add.

begin;

revoke select, insert, update on reports_jobs from tenancy_admin;

drop index if exists reports_jobs_tenant_created_idx;
drop index if exists reports_jobs_tenant_state_idx;

alter table reports_jobs
    drop constraint if exists reports_jobs_state_valid,
    drop constraint if exists reports_jobs_kind_valid,
    drop constraint if exists reports_jobs_format_valid,
    drop constraint if exists reports_jobs_window_ordered;

alter table reports_jobs
    drop column if exists format,
    drop column if exists window_from,
    drop column if exists window_to,
    drop column if exists state,
    drop column if exists started_at,
    drop column if exists bytes_size,
    drop column if exists filename,
    drop column if exists download_url,
    drop column if exists created_by,
    drop column if exists notify_user_id;

-- Restore the legacy id uuid PRIMARY KEY column.
alter table reports_jobs
    drop constraint if exists reports_jobs_pkey;
alter table reports_jobs
    drop column if exists id;
alter table reports_jobs
    add column id uuid default gen_random_uuid();
update reports_jobs set id = gen_random_uuid() where id is null;
alter table reports_jobs
    alter column id set not null,
    add primary key (id);

-- Restore the legacy status + result_s3_key + requested_by columns.
alter table reports_jobs
    add column if not exists status        text,
    add column if not exists result_s3_key text,
    add column if not exists requested_by  uuid;
update reports_jobs set status = 'queued' where status is null;
alter table reports_jobs
    alter column status set not null,
    add constraint reports_jobs_status_check
        check (status in ('queued','running','succeeded','failed'));

-- Add FK on requested_by → users(id) only if the users table exists
-- (defensive — 000001_init creates it before this migration in the
-- forward order).
do $$
begin
    if exists (select 1 from information_schema.tables where table_name = 'users') then
        alter table reports_jobs
            add constraint reports_jobs_requested_by_fkey
            foreign key (requested_by) references users(id);
    end if;
end$$;

-- Restore the legacy queue index.
create index if not exists reports_jobs_queue_idx
    on reports_jobs (status, created_at)
    where status in ('queued','running');

commit;

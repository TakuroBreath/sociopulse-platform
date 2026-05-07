-- 000001_init.down.sql
--
-- Reverse 000001_init.up.sql. Drops policies, tables, role, extensions in
-- the right order. Idempotent where the SQL allows.

begin;

-- Drop policies (defensive; tables go away with CASCADE below anyway).
do $$
declare
  rec record;
begin
  for rec in
    select schemaname, tablename, policyname from pg_policies
    where schemaname = 'public' and policyname like '%_iso'
  loop
    execute format('drop policy %I on %I.%I', rec.policyname, rec.schemaname, rec.tablename);
  end loop;
end$$;

-- Drop tables. Use CASCADE so foreign-key edges go down too.
drop table if exists reports_jobs cascade;
drop table if exists audit_log cascade;
drop table if exists operator_state_log cascade;
drop table if exists operator_sessions cascade;
drop table if exists call_answers cascade;
drop table if exists call_recordings cascade;
drop table if exists call_events cascade;
drop table if exists calls cascade;
drop table if exists survey_versions cascade;
drop table if exists surveys cascade;
drop table if exists project_dnc cascade;
drop table if exists respondents cascade;
drop table if exists project_assignments cascade;
drop table if exists project_quotas cascade;
drop table if exists projects cascade;
drop table if exists user_sessions cascade;
drop table if exists users cascade;
drop table if exists tenant_settings cascade;
drop table if exists tenants cascade;

-- Drop role (only if it's still around; ignore "role does not exist").
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'tenancy_admin') then
    -- revoke memberships first
    execute (select string_agg(format('revoke tenancy_admin from %I', m.member::regrole), '; ')
             from pg_auth_members m where m.roleid = 'tenancy_admin'::regrole);
    drop role tenancy_admin;
  end if;
exception when others then
  -- no-op: ephemeral test environments may not allow role drops; leave behind.
  null;
end$$;

-- Extensions stay (they're fine in a clean DB and dropping requires owner).

commit;

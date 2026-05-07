-- 000003_users_auth_evolve.up.sql
--
-- Plan 05 Task 3 — evolve the existing users table to the auth-module schema:
-- multi-role support (text[]), soft-delete via archived_at, must_change_pwd
-- flag, email column, audit-friendly created_by + updated_at, and a
-- totp_enabled boolean (Plan 06 owns the auth_totp table for the secret).
--
-- The 000001_init migration already created `users` with single-role text +
-- status enum + totp_secret_encrypted bytea, and already enabled RLS with
-- the `users_iso` tenant-isolation policy. This migration is data-preserving
-- where possible: it copies role -> roles[], status='archived' -> archived_at
-- = now(), and only then drops the legacy columns.

begin;

-- ─── new columns ──────────────────────────────────────────────────────────────
alter table users add column if not exists email           text        not null default '';
alter table users add column if not exists must_change_pwd boolean     not null default false;
alter table users add column if not exists updated_at      timestamptz not null default now();
alter table users add column if not exists created_by      uuid        null;
alter table users add column if not exists archived_at     timestamptz null;
alter table users add column if not exists totp_enabled    boolean     not null default false;

-- ─── role  →  roles[] ────────────────────────────────────────────────────────
alter table users add column if not exists roles text[] not null default '{}';

-- Migrate single-role values into the array. Done before the role column drop
-- so existing rows preserve their RBAC identity through the deploy.
update users set roles = array[role] where role is not null and cardinality(roles) = 0;

-- Drop the legacy single-role check + column. The check name follows
-- Postgres' default "<table>_<column>_check" pattern from 000001_init.
alter table users drop constraint if exists users_role_check;
alter table users drop column if exists role;

-- Multi-role invariants:
--  - cardinality > 0 — every user must hold at least one role.
--  - subset of {operator,supervisor,admin} — case-sensitive, matches authapi.Role.
alter table users drop constraint if exists users_roles_nonempty;
alter table users add constraint users_roles_nonempty check (cardinality(roles) > 0);

alter table users drop constraint if exists users_roles_valid;
alter table users add constraint users_roles_valid check (
    roles <@ array['operator','supervisor','admin']::text[]
);

-- ─── status  →  archived_at ──────────────────────────────────────────────────
update users set archived_at = now() where status = 'archived' and archived_at is null;

alter table users drop constraint if exists users_status_check;
alter table users drop column if exists status;

-- ─── drop legacy totp column (Plan 06 owns auth_totp) ────────────────────────
alter table users drop column if exists totp_secret_encrypted;

-- ─── RLS — already enabled in 000001_init.up.sql ─────────────────────────────
-- The `users_iso` policy from 000001_init covers (tenant_id::text =
-- current_setting('app.tenant_id', true)). We keep that policy and simply
-- alias it under the documented Task 3 name without dropping the original
-- so a partial replay does not leave the table without isolation. Idempotent.
do $$
begin
  if not exists (
    select 1 from pg_policies
    where schemaname = 'public' and tablename = 'users' and policyname = 'users_tenant_isolation'
  ) then
    create policy users_tenant_isolation on users
        using (tenant_id::text = current_setting('app.tenant_id', true))
        with check (tenant_id::text = current_setting('app.tenant_id', true));
  end if;
end$$;

-- ─── indexes ─────────────────────────────────────────────────────────────────
create index if not exists idx_users_tenant_active
    on users(tenant_id) where archived_at is null;

create index if not exists idx_users_lower_login
    on users(tenant_id, lower(login)) where archived_at is null;

create index if not exists idx_users_email
    on users(tenant_id, lower(email)) where email <> '';

commit;

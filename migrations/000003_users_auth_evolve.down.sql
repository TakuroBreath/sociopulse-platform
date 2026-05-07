-- 000003_users_auth_evolve.down.sql
--
-- Reverse 000003_users_auth_evolve.up.sql. Restores the legacy single-role
-- text + status text + totp_secret_encrypted bytea shape so 000001_init's
-- definition of `users` is in effect again. Indexes + the auxiliary
-- users_tenant_isolation policy are dropped first to keep the column drops
-- clean.

begin;

-- ─── indexes ─────────────────────────────────────────────────────────────────
drop index if exists idx_users_email;
drop index if exists idx_users_lower_login;
drop index if exists idx_users_tenant_active;

-- ─── auxiliary policy (the 000001 users_iso stays) ───────────────────────────
drop policy if exists users_tenant_isolation on users;

-- ─── restore status text from archived_at ────────────────────────────────────
alter table users add column if not exists status text not null default 'active';
update users set status = 'archived' where archived_at is not null;
alter table users drop constraint if exists users_status_check;
alter table users add constraint users_status_check
    check (status in ('active','archived'));

-- ─── restore single-role text from roles[] ───────────────────────────────────
alter table users add column if not exists role text;
update users set role = roles[1] where role is null and cardinality(coalesce(roles, '{}')) > 0;
alter table users alter column role set not null;
alter table users drop constraint if exists users_roles_valid;
alter table users drop constraint if exists users_roles_nonempty;
alter table users drop column if exists roles;
alter table users drop constraint if exists users_role_check;
alter table users add constraint users_role_check
    check (role in ('operator','supervisor','admin'));

-- ─── restore legacy totp column (nullable bytea) ─────────────────────────────
alter table users add column if not exists totp_secret_encrypted bytea;

-- ─── drop columns added in up ────────────────────────────────────────────────
alter table users drop column if exists totp_enabled;
alter table users drop column if exists archived_at;
alter table users drop column if exists created_by;
alter table users drop column if exists updated_at;
alter table users drop column if exists must_change_pwd;
alter table users drop column if exists email;

commit;

-- 000004_auth_totp.up.sql
--
-- Plan 05 Task 6 — per-user TOTP enrolment table. Owns the TOTP secret
-- (encrypted with the per-tenant DEK obtained from tenancy.KMSResolver),
-- backup-code hashes (Argon2id, single-use), enrolment status, and
-- last-verified timestamp. The users.totp_enabled boolean stays as the
-- fast-path flag the Authenticator reads on the login fork (Plan 05
-- Task 4 Step 12); this table holds the secret + verification trail.
--
-- Why a separate table:
--   - The secret is bytea; users is row-narrow. Splitting the encrypted
--     payload off keeps users projections cheap.
--   - On Disable we DELETE the row outright — no lingering encrypted
--     bytes after a user opts out. That guarantee is harder when the
--     column lives on users.
--
-- RLS: tenant-scoped. Every read/write goes through Pool.WithTenant,
-- which sets app.tenant_id. The composition root never reaches in
-- through BypassRLS for TOTP rows.
--
-- Idempotency: CREATE TABLE / POLICY / INDEX use IF NOT EXISTS where
-- supported; the policy block is wrapped in a DO $$ guard so a partial
-- replay does not error on the existing policy.

begin;

create table if not exists auth_totp (
    user_id            uuid primary key references users(id) on delete cascade,
    tenant_id          uuid not null references tenants(id) on delete cascade,
    secret_enc         bytea not null,                          -- AES-GCM via per-tenant DEK
    enrolled           boolean not null default false,
    enrolled_at        timestamptz,
    last_verified_at   timestamptz,
    backup_codes_hash  text[] not null default '{}',            -- Argon2id PHC strings; single-use removal on use
    backup_used_count  integer not null default 0,
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now()
);

alter table auth_totp enable row level security;

do $$
begin
  if not exists (
    select 1 from pg_policies
    where schemaname = 'public' and tablename = 'auth_totp' and policyname = 'auth_totp_tenant_isolation'
  ) then
    create policy auth_totp_tenant_isolation on auth_totp
        using (tenant_id::text = current_setting('app.tenant_id', true))
        with check (tenant_id::text = current_setting('app.tenant_id', true));
  end if;
end$$;

create index if not exists idx_auth_totp_tenant
    on auth_totp(tenant_id) where enrolled;

commit;

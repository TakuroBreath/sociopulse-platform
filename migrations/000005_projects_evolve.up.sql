-- 000005_projects_evolve.up.sql
--
-- Plan 06 Task 1 — evolve the existing projects table to the crm-module schema:
-- is_advertising flag (rejected in v1 per ErrAdvertisingRejected; we still add
-- the column so future tasks can flip it without another migration), audit-
-- friendly created_by + updated_at, and archived_at for soft-delete.
--
-- The 000001_init migration already created `projects` with id, tenant_id,
-- code, name, customer, status, target_count, period_from, period_to,
-- survey_id, default_survey_version_id, created_at, UNIQUE(tenant_id, code),
-- and already enabled RLS via the `projects_iso` tenant-isolation policy.
-- This migration is purely additive; no data is mutated.

begin;

-- ─── new columns ──────────────────────────────────────────────────────────────
alter table projects add column if not exists is_advertising boolean     not null default false;
alter table projects add column if not exists created_by     uuid        null;
alter table projects add column if not exists updated_at     timestamptz not null default now();
alter table projects add column if not exists archived_at    timestamptz null;

-- ─── indexes ─────────────────────────────────────────────────────────────────
-- Hot path is "list active projects per tenant" — admin dashboards filter by
-- (tenant_id, status) and ignore archived rows. The partial predicate keeps
-- the index small.
create index if not exists idx_projects_tenant_status
    on projects(tenant_id, status) where archived_at is null;

-- Case-insensitive lookup by code per tenant. The unique constraint already
-- enforces (tenant_id, code) collision; this functional index supports
-- GetByCode(lower(...)) when the API surfaces case-insensitive matching.
create index if not exists idx_projects_tenant_code_lower
    on projects(tenant_id, lower(code)) where archived_at is null;

commit;

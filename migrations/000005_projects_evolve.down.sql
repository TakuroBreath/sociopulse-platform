-- 000005_projects_evolve.down.sql
--
-- Reverse 000005_projects_evolve.up.sql. Drops the auxiliary indexes and the
-- columns added by the up migration. The columns are nullable / defaulted so
-- the rollback never destroys row identity — the projects table simply
-- returns to the 000001_init shape (without is_advertising/created_by/
-- updated_at/archived_at).
--
-- Idempotent: every drop uses IF EXISTS.

begin;

-- ─── indexes ─────────────────────────────────────────────────────────────────
drop index if exists idx_projects_tenant_code_lower;
drop index if exists idx_projects_tenant_status;

-- ─── columns ─────────────────────────────────────────────────────────────────
alter table projects drop column if exists archived_at;
alter table projects drop column if exists updated_at;
alter table projects drop column if exists created_by;
alter table projects drop column if exists is_advertising;

commit;

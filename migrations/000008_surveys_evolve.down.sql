-- 000008_surveys_evolve.down.sql
--
-- Reverse 000008_surveys_evolve.up.sql. Drops the auxiliary indexes and the
-- columns added by the up migration.
--
-- DESTRUCTIVE NOTICE: dropping `status`, `major`, `minor`, `description`,
-- `archived_at` is data-destructive on a populated database — every value
-- in those columns is lost on rollback. The rollback policy here is
-- DEV-ONLY: production clusters must take a logical backup before running
-- `migrate down` against this migration. CI/integration tests carry no
-- production data and rerun migrations from scratch, so the destructive
-- nature is acceptable for them.
--
-- Idempotent: every drop uses IF EXISTS.

begin;

-- ─── indexes ─────────────────────────────────────────────────────────────────
drop index if exists survey_versions_unique_per_survey;
drop index if exists idx_surveys_tenant_name_lower;
drop index if exists idx_surveys_tenant_status;

-- ─── survey_versions: columns ────────────────────────────────────────────────
alter table survey_versions drop column if exists activated_at;
alter table survey_versions drop column if exists minor;
alter table survey_versions drop column if exists major;

-- ─── surveys: columns ────────────────────────────────────────────────────────
alter table surveys drop column if exists archived_at;
alter table surveys drop column if exists created_by;
alter table surveys drop column if exists updated_at;
alter table surveys drop column if exists status;
alter table surveys drop column if exists description;

commit;

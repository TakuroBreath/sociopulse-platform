-- 000007_respondents_deletion.down.sql
--
-- Reverse 000007_respondents_deletion.up.sql. Drops the index and the
-- nullable columns added by the up migration. Both columns are nullable
-- so the rollback never destroys row identity — the respondents table
-- simply returns to the pre-deletion-grace shape.
--
-- Note: dropping deleted_at is data-destructive when soft-deleted rows
-- exist. We accept that here because the down direction is for local
-- dev / CI revert, never for production rollback (production runs
-- forward-only migrations, per project policy).
--
-- Idempotent: every drop uses IF EXISTS.

begin;

drop index if exists idx_respondents_deleted;

alter table respondents drop column if exists deletion_reason;
alter table respondents drop column if exists deleted_at;

commit;

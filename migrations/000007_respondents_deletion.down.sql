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

-- Restore the original (narrower) status CHECK before any rows can
-- carry the 'deletion-requested' value. If any row currently has that
-- status, the rollback will fail at constraint creation — operators
-- must `UPDATE respondents SET status = 'pending' WHERE status = 'deletion-requested'`
-- (and accept the loss of the deletion-requested signal) before
-- migrating down. We do NOT silently rewrite rows here.
alter table respondents drop constraint if exists respondents_status_check;
alter table respondents add constraint respondents_status_check
    check (status in ('pending','dialing','completed','dnc','exhausted','wrong'));

drop index if exists idx_respondents_deleted;

alter table respondents drop column if exists deletion_reason;
alter table respondents drop column if exists deleted_at;

commit;

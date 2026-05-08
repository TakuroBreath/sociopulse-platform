-- 000006_respondents_uniq.down.sql
--
-- Reverse 000006_respondents_uniq.up.sql: drop the per-(tenant, project,
-- phone_hash) unique constraint added by the up migration. Idempotent
-- via IF EXISTS so a partial rollback never wedges.
--
-- This is data-safe — dropping a unique constraint can never destroy
-- rows, and the underlying B-tree index PostgreSQL maintained for the
-- constraint is removed automatically as part of the constraint drop.

begin;

alter table respondents
    drop constraint if exists respondents_tenant_project_phone_hash_uniq;

commit;

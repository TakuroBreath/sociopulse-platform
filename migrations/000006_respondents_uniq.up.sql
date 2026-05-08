-- 000006_respondents_uniq.up.sql
--
-- Plan 06 Task 3 — enforce respondent uniqueness within a project.
--
-- The 000001_init migration created `respondents` with a non-unique index
-- on (project_id, phone_hash). This migration promotes that pair to a
-- proper unique constraint scoped to (tenant_id, project_id, phone_hash)
-- so duplicate respondents within a project are caught at the DB level
-- and surfaced to the service layer as ErrDuplicateRespondent (SQLSTATE
-- 23505 → translated by the store).
--
-- The constraint is tenant-scoped via tenant_id in the leading position
-- so RLS-bypassed admin queries still see one row per (tenant, project,
-- phone), and PgBouncer-friendly transaction-mode connections reuse the
-- same B-tree as the existing respondents_phone_hash_idx for hash-only
-- lookups (PostgreSQL is happy to use a multi-column index when the
-- leading columns are equality-bound).
--
-- The non-unique respondents_phone_hash_idx is left in place: the new
-- unique index covers (tenant, project, hash), but search by hash alone
-- (e.g. cross-project DNC checks if we ever surface them) still benefits
-- from the project-then-hash leading order.

begin;

alter table respondents
    add constraint respondents_tenant_project_phone_hash_uniq
    unique (tenant_id, project_id, phone_hash);

commit;

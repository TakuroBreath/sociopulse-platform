-- 000011_admin_grants_call_recordings.up.sql
--
-- Plan 12.4 Task 1 — extend tenancy_admin privileges so the recording
-- lifecycle workers (retention sweep + integrity verifier) can scan and
-- mutate call_recordings cross-tenant via postgres.Pool.BypassRLS.
--
-- Background: 000001_init grants tenancy_admin (BYPASSRLS at the role
-- level) DML only on the tables the tenancy module owns directly. When a
-- regular app-user session does SET LOCAL ROLE tenancy_admin (the
-- BypassRLS path in pkg/postgres.Pool), it swaps identity and the
-- original user's grants no longer apply — so a SELECT on call_recordings
-- fails with 42501 permission_denied, defeating the BypassRLS purpose.
--
-- 000009_admin_grants_respondents established the same pattern for the
-- dialer-retry orchestrator on respondents/project_dnc/projects.
--
-- The fix here mirrors that pattern for the recording module:
--
--   - call_recordings: SELECT (cross-tenant cold-move + delete sweep, plus
--     TABLESAMPLE BERNOULLI for the integrity verifier), UPDATE (status
--     CAS to 'cold'/'deleted', verified_at + integrity_ok writebacks).
--     DELETE is NOT granted — the retention pipeline marks status='deleted'
--     and the S3 object is purged separately; the row is retained as an
--     audit trail until a future archival migration.
--
-- We deliberately keep the grant set tight — tenancy_admin is the shared
-- "platform-internal" role; widening it to ALL TABLES would give
-- cross-tenant write access to surveys / calls / outbox by accident.
-- Each future BypassRLS consumer adds the table here.
--
-- Idempotent: GRANT is idempotent in Postgres — re-running the migration
-- after partial failure is safe.

begin;

grant select, update on call_recordings to tenancy_admin;

commit;

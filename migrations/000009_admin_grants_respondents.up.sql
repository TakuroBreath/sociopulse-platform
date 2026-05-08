-- 000009_admin_grants_respondents.up.sql
--
-- Plan 10 Task 8 — extend tenancy_admin privileges so the dialer
-- retry orchestrator and the crm purge worker can scan / mutate
-- respondents cross-tenant via postgres.Pool.BypassRLS.
--
-- Background: 000001_init grants tenancy_admin (BYPASSRLS at the role
-- level) DML only on the tables the tenancy module owns directly
-- (tenants, tenant_settings). When a regular app-user session does
-- SET LOCAL ROLE tenancy_admin (the BypassRLS path in
-- pkg/postgres.Pool), it swaps identity and the original user's
-- grants no longer apply — so a SELECT on respondents fails with
-- 42501 permission_denied, defeating the BypassRLS purpose.
--
-- The fix is to grant tenancy_admin the privileges it actually needs
-- for the platform's cross-tenant operations:
--
--   - respondents: SELECT (scan mature retries), UPDATE (mark
--     scheduled / exhausted by the retry orchestrator), DELETE
--     (PurgeWorker hard-deletes after the 30-day grace).
--   - project_dnc: SELECT (DNC checks during retry), INSERT (mark
--     wrong-person rows on terminal disposition).
--   - projects: SELECT (resolve project context cross-tenant for
--     telemetry).
--
-- We deliberately keep the grant set tight — tenancy_admin is the
-- shared "platform-internal" role; widening it to ALL TABLES would
-- give cross-tenant write access to surveys / calls / outbox by
-- accident. Each future BypassRLS consumer adds the table here.
--
-- Idempotent: GRANT is idempotent in Postgres — re-running the
-- migration after partial failure is safe.

begin;

grant select, update, delete on respondents to tenancy_admin;
grant select, insert on project_dnc to tenancy_admin;
grant select on projects to tenancy_admin;

commit;

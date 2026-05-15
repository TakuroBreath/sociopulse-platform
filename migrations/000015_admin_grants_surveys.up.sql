-- 000015_admin_grants_surveys.up.sql
--
-- Plan 21b Task 2 surfaced this gap — extend tenancy_admin privileges so
-- the surveys module's cross-tenant tenant-resolution lookup (used by the
-- pkg/middleware/tenant.RequireSameTenant guard wired on every survey
-- write endpoint) can succeed through postgres.Pool.BypassRLS.
--
-- Background: 000001_init grants tenancy_admin (BYPASSRLS at the role
-- level) DML only on the tables the tenancy module owns directly. When a
-- regular app-user session does SET LOCAL ROLE tenancy_admin (the
-- BypassRLS path in pkg/postgres.Pool), it swaps identity and the
-- original user's grants no longer apply — so a SELECT on surveys fails
-- with 42501 permission_denied, defeating the BypassRLS purpose. The
-- surveys handler then returns a 500 to the caller (the tenant-resolver
-- middleware translates "non-ErrNotFound resolver error" to 500 by
-- design — it deliberately does NOT leak the underlying message).
--
-- 000009 / 000011 / 000014 established the same pattern for respondents
-- / call_recordings / calls. The surveys grant was missed when
-- Plan 13.2.5 Task 1 introduced the surveys-side RequireSameTenant
-- chain; handler unit tests passed because they use a fakeSurveyService
-- that never hits Postgres. Plan 21b Task 2's first end-to-end smoke
-- scenario for surveys exposes the production bug — the fix is this
-- one-line grant pair.
--
-- The grant set:
--
--   - surveys:         SELECT only (ResolveTenant reads the row to
--                      project tenant_id; no DML cross-tenant path
--                      exists today).
--   - survey_versions: SELECT only (future store-side BypassRLS
--                      paths — e.g. cross-tenant analytics readers
--                      — will read the version rows alongside the
--                      survey row; grant in lockstep keeps the role
--                      surface coherent).
--
-- We deliberately keep the grant set tight — tenancy_admin is the
-- shared "platform-internal" role; widening it to ALL TABLES would
-- give cross-tenant write access to call_costs / outbox / audit_log by
-- accident. Each future BypassRLS consumer adds the table here.
--
-- Idempotent: GRANT is idempotent in Postgres — re-running the
-- migration after a partial failure is safe.

begin;

grant select on surveys         to tenancy_admin;
grant select on survey_versions to tenancy_admin;

commit;

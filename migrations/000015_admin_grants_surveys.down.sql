-- 000015_admin_grants_surveys.down.sql
--
-- Reverse the grants added by 000015_admin_grants_surveys.up.sql.
-- After this migration runs, tenancy_admin no longer has SELECT on
-- surveys / survey_versions — the surveys-side RequireSameTenant
-- middleware will return 500 on every cross-tenant resolution, which
-- is the pre-Plan-21b-Task-2 state.

begin;

revoke select on survey_versions from tenancy_admin;
revoke select on surveys         from tenancy_admin;

commit;

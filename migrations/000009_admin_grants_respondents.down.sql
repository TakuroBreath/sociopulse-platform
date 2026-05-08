-- 000009_admin_grants_respondents.down.sql
--
-- Reverse of 000009_admin_grants_respondents.up.sql.
--
-- After this migration runs, tenancy_admin no longer has SELECT /
-- UPDATE / DELETE on respondents — the retry orchestrator and purge
-- worker will fail with permission denied on their next sweep. Down
-- migrations are reserved for revert flows; production rollbacks
-- should redeploy the application alongside.

begin;

revoke select, update, delete on respondents from tenancy_admin;
revoke select, insert on project_dnc from tenancy_admin;
revoke select on projects from tenancy_admin;

commit;

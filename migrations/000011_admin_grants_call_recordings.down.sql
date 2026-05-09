-- 000011_admin_grants_call_recordings.down.sql
--
-- Reverse the grants added by 000011_admin_grants_call_recordings.up.sql.
-- After this migration runs, tenancy_admin no longer has SELECT / UPDATE
-- on call_recordings — the recording lifecycle workers cannot run via
-- BypassRLS, which is the pre-Plan-12.4 state.

begin;

revoke select, update on call_recordings from tenancy_admin;

commit;

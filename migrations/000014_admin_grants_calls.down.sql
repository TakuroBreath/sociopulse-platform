-- 000014_admin_grants_calls.down.sql
--
-- Reverse of 000014_admin_grants_calls.up.sql.
--
-- After this migration runs, tenancy_admin no longer has SELECT on
-- the calls table, so the dialer transport's CallTenantResolver
-- (BypassRLS read) will fail with 42501 permission_denied — disabling
-- the Plan 21 Task 3 cross-tenant guard on POST /api/calls/:id/hangup
-- until 000014 is re-applied. Only run this in a tenant where the
-- guard is being deliberately rolled back (test fixtures, schema-
-- evolution dry runs).

begin;

revoke select on calls from tenancy_admin;

commit;

-- 000014_admin_grants_calls.up.sql
--
-- Plan 21 Task 3 — extend tenancy_admin privileges so the dialer
-- transport's CallTenantResolver (BypassRLS read on the calls table)
-- can answer the pkg/middleware/tenant.RequireSameTenant guard on
-- POST /api/calls/:id/hangup. Closes the Plan 13.2.5 out-of-scope
-- cross-tenant hangup finding tracked for v0.0.26.
--
-- Background: 000001_init grants tenancy_admin (BYPASSRLS at the role
-- level) DML only on the tables the tenancy module owns directly. When
-- a regular app-user session does SET LOCAL ROLE tenancy_admin (the
-- BypassRLS path in pkg/postgres.Pool), it swaps identity and the
-- original user's grants no longer apply — so a SELECT on calls fails
-- with 42501 permission_denied, defeating the BypassRLS purpose.
--
-- 000009_admin_grants_respondents established the same pattern for
-- the dialer-retry orchestrator on respondents/project_dnc/projects;
-- 000011_admin_grants_call_recordings extended it to call_recordings
-- for the recording lifecycle workers and the realtime CallResolver.
-- This migration is the symmetric extension for the calls table.
--
-- The fix here is narrow:
--
--   - calls: SELECT (cross-tenant tenant_id lookup is the input to
--     the RequireSameTenant guard on /api/calls/:id/hangup). UPDATE /
--     INSERT / DELETE are NOT granted — the call's lifecycle stays
--     under WithTenant RLS-scoped writes (FSM transitions,
--     telephony-bridge event ingest). The BypassRLS reader is
--     read-only by design.
--
-- We deliberately keep the grant set tight — tenancy_admin is the
-- shared "platform-internal" role; widening it to ALL TABLES would
-- give cross-tenant write access to surveys / outbox / etc. by
-- accident. Each future BypassRLS consumer adds the table here.
--
-- Idempotent: GRANT is idempotent in Postgres — re-running the
-- migration after partial failure is safe.

begin;

grant select on calls to tenancy_admin;

commit;

-- 000013_billing.up.sql
--
-- Plan 14 Step A — billing module foundation.
--
-- This migration delivers two related changes in one pair (per
-- docs/references/plan-14-billing.md §4.12):
--
--   Part 1: call_costs — denormalised per-call cost row, written by
--           billing.OnCallFinalized (Step D). Idempotent on call_id
--           via ON CONFLICT DO NOTHING; redelivery of the
--           dialer.call.finalized NATS event is safe.
--   Part 2: projects.contract_fee_per_completed_minor — per-completed-
--           survey revenue contract fee (kopecks), consumed by the
--           RevenueCalculator (Step F).
--
-- RLS: call_costs gets the canonical tenant_isolation policy
-- (tenant_id = current_setting('app.tenant_id')::uuid). tenancy_admin
-- gets SELECT/INSERT/UPDATE so future cross-tenant analytics jobs
-- (recompute on tariff drift, retention sweeps) can run under
-- pkg/postgres.Pool.BypassRLS — mirrors the Plan 12.4 pattern for
-- call_recordings (migration 000011).
--
-- All money values are bigint kopecks (int64 in Go) with CHECK (>= 0).
-- Storage cost is captured per-call at finalize time; the monthly re-
-- charge variant is explicitly deferred to v2 per Plan 14 §4.4.

begin;

-- ─── Part 1: call_costs ──────────────────────────────────────────────────────
create table call_costs (
  call_id          uuid        primary key references calls(id) on delete cascade,
  tenant_id        uuid        not null references tenants(id) on delete cascade,
  project_id       uuid        not null references projects(id) on delete cascade,

  -- Inputs captured at calculation time (audit trail).
  trunk_used       text,                                         -- nullable: missing trunk → telecom_minor=0
  duration_sec     int         not null default 0,
  status           text        not null,                         -- calls.status at finalize time

  -- Cost components (kopecks).
  telecom_minor    bigint      not null default 0 check (telecom_minor >= 0),
  wages_minor      bigint      not null default 0 check (wages_minor   >= 0),
  storage_minor    bigint      not null default 0 check (storage_minor >= 0),
  total_minor      bigint      not null default 0 check (total_minor   >= 0),

  -- Tariff snapshot identifier (int counter, matches api.Tariffs.Version).
  tariff_version   integer,                                      -- nullable for legacy rows; filled on insert when known
  finalized_at     timestamptz not null default now(),
  computed_at      timestamptz not null default now()
);

create index call_costs_tenant_finalized
  on call_costs (tenant_id, finalized_at desc);

create index call_costs_project_finalized
  on call_costs (project_id, finalized_at desc);

alter table call_costs enable row level security;

create policy call_costs_tenant_isolation on call_costs
  using (tenant_id = current_setting('app.tenant_id')::uuid);

-- tenancy_admin BYPASSRLS grants for future cross-tenant analytics /
-- recompute jobs (mirrors Plan 12.4 / migration 000011 for call_recordings).
grant select, insert, update on call_costs to tenancy_admin;

-- App user explicit grant — the wholesale grant in 000001_init only
-- applied to tables existing at that moment; new tables in later
-- migrations need their own grant when the migrator runs as the table
-- owner (current_user == app in dev/prod). DELETE intentionally
-- omitted: per-call rows are retained as audit trail; future archival
-- jobs run under tenancy_admin.
do $$
declare
  app_user text;
begin
  app_user := current_user;
  execute format('grant select, insert, update on call_costs to %I', app_user);
end$$;

comment on table call_costs is
  'Denormalised per-call cost components (kopecks). Written by billing.OnCallFinalized handler. Idempotent on call_id.';
comment on column call_costs.tariff_version is
  'Snapshot of api.Tariffs.Version at calculation time (monotonic integer). Used by future recompute job to detect drift.';

-- ─── Part 2: projects.contract_fee_per_completed_minor ───────────────────────
alter table projects
  add column if not exists contract_fee_per_completed_minor bigint not null default 0;

comment on column projects.contract_fee_per_completed_minor is
  'Per-completed-survey fee paid by the customer (kopecks). 0 = no contract attached.';

commit;

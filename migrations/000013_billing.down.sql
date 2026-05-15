-- 000013_billing.down.sql
--
-- Reverse Plan 14 Step A. Drops in the opposite order of up.sql so
-- foreign-key references are satisfied throughout.
--
-- WARNING: dropping projects.contract_fee_per_completed_minor discards
-- the per-project revenue configuration. Production rollback is a fire
-- drill — operators should snapshot the column first if they care
-- about preserving fees across the rollback window.

begin;

alter table projects drop column if exists contract_fee_per_completed_minor;

drop policy if exists call_costs_tenant_isolation on call_costs;
drop index   if exists call_costs_project_finalized;
drop index   if exists call_costs_tenant_finalized;
drop table   if exists call_costs;

commit;

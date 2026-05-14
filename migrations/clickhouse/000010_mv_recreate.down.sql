-- Drop the recreated MV definitions. The .up.sql's matching
-- DOWN-migrations 000009/008/007 (in reverse order) re-drop them
-- before renaming the new tables back to legacy, so this DOWN need
-- only undo this migration step itself.

DROP VIEW IF EXISTS mv_quotas_progress;
DROP VIEW IF EXISTS mv_operator_kpi_daily_states;
DROP VIEW IF EXISTS mv_operator_kpi_daily_calls;
DROP VIEW IF EXISTS mv_calls_hourly

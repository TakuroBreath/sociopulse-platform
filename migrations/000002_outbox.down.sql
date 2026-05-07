-- 000002_outbox.down.sql
--
-- Reverse 000002_outbox.up.sql.

begin;

drop table if exists event_outbox cascade;

commit;

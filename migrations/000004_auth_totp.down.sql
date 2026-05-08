-- 000004_auth_totp.down.sql
--
-- Reverse 000004_auth_totp.up.sql. Dropping the table also drops its
-- policy and index in cascade, so we do not need separate DROP POLICY /
-- DROP INDEX statements. users.totp_enabled is owned by 000003 and
-- stays in place.

begin;

drop table if exists auth_totp;

commit;

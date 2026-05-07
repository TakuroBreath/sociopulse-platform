-- 000002_outbox.up.sql
--
-- Transactional outbox table. The relay in pkg/outbox drains pending rows
-- to NATS using FOR UPDATE SKIP LOCKED, so it is safe to run on every
-- cmd/api replica without leader election.
--
-- See spec §16 (events) and Plan 03 Task 6 for the full rationale.

begin;

create table event_outbox (
  id            bigserial primary key,
  tenant_id     uuid null,                       -- nullable for platform-global events
  aggregate_id  uuid null,                       -- e.g. operator_id, call_id, recording_id
  subject       text not null,                   -- canonical NATS subject, e.g. tenant.<tid>.dialer.op.<op>.state
  payload       jsonb not null,
  created_at    timestamptz not null default now(),
  published_at  timestamptz null,
  attempts      integer not null default 0,
  last_error    text null
);

-- Index for the relay drain query (unpublished, oldest first).
create index event_outbox_pending_idx
  on event_outbox (created_at)
  where published_at is null;

-- Index for housekeeping (delete published rows older than retention).
create index event_outbox_published_idx
  on event_outbox (published_at)
  where published_at is not null;

-- Outbox is platform-internal infra; not subject to RLS — it's owned by
-- tenancy_admin so the relay (which runs under that role via BypassRLS)
-- can drain across all tenants. The application user retains CRUD so
-- inserts from inside per-tenant transactions still succeed.
do $$
declare
  app_user text;
begin
  app_user := current_user;
  -- tenancy_admin owns the table so the BypassRLS relay can SELECT FOR
  -- UPDATE without policy interference. We re-grant CRUD to the app user.
  alter table event_outbox owner to tenancy_admin;
  execute format('grant insert, select, update, delete on event_outbox to %I', app_user);
  execute format('grant usage, select on sequence event_outbox_id_seq to %I', app_user);
end$$;

commit;

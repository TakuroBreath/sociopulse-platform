-- 000007_respondents_deletion.up.sql
--
-- Plan 06 Task 5 — extend respondents with the 152-ФЗ subject right to
-- deletion (Статья 21 152-ФЗ): every respondent must be removable on
-- subject request. Implementation strategy:
--   1. Soft-delete column (deleted_at) — set when the user requests
--      deletion. The row stays visible to admin tooling for a 30-day
--      grace window so an accidental delete is reversible.
--   2. Optional reason column captures the human-readable reason
--      (free-form, displayed in the audit_log). NULL means
--      "user_request" by default.
--   3. Partial index targeting WHERE deleted_at IS NOT NULL — keeps
--      the index small (most rows are NULL) and lets the daily purge
--      worker (cmd/worker.respondents.purge) scan only candidates.
--
-- Idempotent: every clause is guarded with IF NOT EXISTS so a re-run
-- after partial failure leaves the schema in the desired state.

begin;

alter table respondents
    add column if not exists deleted_at timestamptz;

alter table respondents
    add column if not exists deletion_reason text;

create index if not exists idx_respondents_deleted
    on respondents(deleted_at)
    where deleted_at is not null;

-- Extend the status CHECK to include the new 'deletion-requested'
-- value the SoftDelete writer flips rows to alongside deleted_at.
-- The check is named `respondents_status_check` by Postgres' auto-
-- naming for inline CHECK constraints in 000001_init; we drop the
-- anonymous check and re-create with the wider value set so the
-- migration is idempotent.
alter table respondents drop constraint if exists respondents_status_check;
alter table respondents add constraint respondents_status_check
    check (status in ('pending','dialing','completed','dnc','exhausted','wrong','deletion-requested'));

commit;

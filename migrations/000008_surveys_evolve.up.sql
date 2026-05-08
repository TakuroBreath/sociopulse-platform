-- 000008_surveys_evolve.up.sql
--
-- Plan 07 Task 4 — evolve the existing surveys / survey_versions tables to
-- the surveys-module schema:
--   * surveys: description, status, updated_at, created_by, archived_at
--   * survey_versions: major, minor, activated_at; version_label kept (legacy)
--     and backfilled to "<major>.<minor>" for any pre-existing rows.
--   * indexes: tenant_status partial, tenant_lower(name) partial, and
--     unique (survey_id, major, minor).
--
-- The 000001_init migration created surveys + survey_versions with
-- (id, tenant_id, name, current_version_id, primary_mode, created_at) and
-- (id, tenant_id, survey_id, version_label not null, schema, is_active,
-- created_at, created_by) plus the partial unique index
-- survey_versions_active_one and RLS via surveys_iso/survey_versions_iso.
-- Plan 07 Task 4 layers on the columns the api package needs to project a
-- full Survey/Version DTO.
--
-- version_label handling: the column is NOT NULL with no default in 000001.
-- This migration does NOT drop it (kept for backwards compatibility with
-- any seed/test data that wrote it directly), but redefines its semantics
-- to a derived string of the form "<major>.<minor>". A pre-existing row
-- without major/minor takes the new defaults (1.0). The store layer in
-- Task 4 always writes version_label = "<major>.<minor>" to keep both
-- columns in sync.

begin;

-- ─── surveys: new columns ─────────────────────────────────────────────────────
alter table surveys add column if not exists description text        not null default '';
alter table surveys add column if not exists status      text        not null default 'active'
    check (status in ('active','archived'));
alter table surveys add column if not exists updated_at  timestamptz not null default now();
alter table surveys add column if not exists created_by  uuid        references users(id);
alter table surveys add column if not exists archived_at timestamptz;

-- ─── survey_versions: new columns ─────────────────────────────────────────────
alter table survey_versions add column if not exists major        int         not null default 1;
alter table survey_versions add column if not exists minor        int         not null default 0;
alter table survey_versions add column if not exists activated_at timestamptz;

-- Backfill version_label to "<major>.<minor>" for any rows with empty values.
-- (No-op in dev/CI; rows that pre-date Task 4 carry the legacy free-form
-- label.) The store going forward writes both columns identically.
update survey_versions
   set version_label = major::text || '.' || minor::text
 where version_label is null
    or version_label = '';

-- ─── indexes ─────────────────────────────────────────────────────────────────
-- Hot path is "list active surveys per tenant" — admin dashboards filter by
-- (tenant_id, status) and ignore archived rows. Partial predicate keeps the
-- index small.
create index if not exists idx_surveys_tenant_status
    on surveys(tenant_id, status) where archived_at is null;

-- Case-insensitive search by name per tenant for the admin search box.
create index if not exists idx_surveys_tenant_name_lower
    on surveys(tenant_id, lower(name)) where archived_at is null;

-- Per-survey (major, minor) is unique. Catches accidental duplicate
-- SaveVersion at the DB level even when the service-layer max-major+1 logic
-- races against itself (two concurrent SaveVersion calls would both compute
-- the same next minor; this index makes the second INSERT fail with 23505).
create unique index if not exists survey_versions_unique_per_survey
    on survey_versions(survey_id, major, minor);

commit;

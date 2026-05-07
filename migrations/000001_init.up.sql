-- 000001_init.up.sql
--
-- Initial schema for СоциоПульс.
-- Covers spec §6.3 (every business table), §6.1 (RLS), §12.2 (RLS policies +
-- tenancy_admin role with BYPASSRLS).
--
-- Idempotency: this file uses CREATE EXTENSION IF NOT EXISTS but otherwise
-- assumes a clean target. The 000001_init.down.sql reverses everything.

begin;

-- ─── extensions ───────────────────────────────────────────────────────────────
create extension if not exists pgcrypto;          -- gen_random_uuid()
create extension if not exists "uuid-ossp";       -- legacy UUID functions for any tools
create extension if not exists pg_trgm;           -- similarity searches (used later by reports)

-- ─── roles ────────────────────────────────────────────────────────────────────
-- tenancy_admin is used by the `tenancy` module for cross-tenant operations
-- (see spec §6.1). The role is granted to the application user but only
-- assumed inside the tenancy module via SET ROLE.
do $$
begin
  if not exists (select 1 from pg_roles where rolname = 'tenancy_admin') then
    create role tenancy_admin bypassrls;
  end if;
end$$;

-- The application connects as the (already-existing) `app` user in production.
-- In ephemeral test containers the user is named per Postgres image defaults;
-- use a permissive grant to make tests portable.
do $$
declare
  app_user text;
begin
  app_user := current_user;
  execute format('grant tenancy_admin to %I', app_user);
end$$;

-- ─── tenancy ──────────────────────────────────────────────────────────────────
create table tenants (
  id uuid primary key default gen_random_uuid(),
  org_code text not null unique,
  name text not null,
  status text not null check (status in ('active','suspended','archived')),
  kms_kek_id text not null,
  phone_hash_pepper bytea not null,
  created_at timestamptz not null default now()
);

create table tenant_settings (
  tenant_id uuid not null references tenants(id) on delete cascade,
  key text not null,
  value jsonb not null,
  updated_at timestamptz not null default now(),
  primary key (tenant_id, key)
);

-- ─── auth ─────────────────────────────────────────────────────────────────────
create table users (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id),
  login text not null,
  password_hash text not null,
  full_name text not null,
  role text not null check (role in ('operator','supervisor','admin')),
  status text not null check (status in ('active','archived')),
  totp_secret_encrypted bytea,
  hired_at date,
  last_login_at timestamptz,
  created_at timestamptz not null default now(),
  unique (tenant_id, login)
);

create table user_sessions (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  user_id uuid not null references users(id),
  refresh_token_hash text not null,
  expires_at timestamptz not null,
  ip text,
  user_agent text,
  revoked_at timestamptz,
  created_at timestamptz not null default now()
);
create index user_sessions_active_idx
  on user_sessions (user_id, expires_at)
  where revoked_at is null;

-- ─── crm ──────────────────────────────────────────────────────────────────────
create table projects (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id),
  code text not null,
  name text not null,
  customer text,
  status text not null check (status in ('active','paused','archived')),
  target_count int not null default 0,
  period_from date,
  period_to date,
  survey_id uuid,
  default_survey_version_id uuid,
  created_at timestamptz not null default now(),
  unique (tenant_id, code)
);

create table project_quotas (
  project_id uuid not null references projects(id) on delete cascade,
  dimension_kind text not null
    check (dimension_kind in ('region','gender','age_bucket','custom')),
  dimension_value text not null,
  target int not null check (target >= 0),
  done int not null default 0 check (done >= 0),
  primary key (project_id, dimension_kind, dimension_value)
);

create table project_assignments (
  project_id uuid not null references projects(id) on delete cascade,
  operator_id uuid not null references users(id),
  assigned_at timestamptz not null default now(),
  primary key (project_id, operator_id)
);

create table respondents (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  project_id uuid not null references projects(id),
  phone_encrypted bytea not null,
  phone_hash bytea not null,
  region_code text not null,
  attributes jsonb not null default '{}'::jsonb,
  status text not null default 'pending'
    check (status in ('pending','dialing','completed','dnc','exhausted','wrong')),
  attempts int not null default 0,
  last_attempt_at timestamptz,
  next_attempt_at timestamptz,
  source text not null check (source in ('imported','rdd')),
  created_at timestamptz not null default now()
);
create index respondents_due_idx
  on respondents (project_id, status, next_attempt_at);
create index respondents_phone_hash_idx
  on respondents (project_id, phone_hash);

create table project_dnc (
  tenant_id uuid not null,
  project_id uuid,
  phone_hash bytea not null,
  source text not null check (source in ('manual','import','wrong-person','tenant-wide')),
  added_at timestamptz not null default now()
);
-- Postgres does not allow function expressions inside PRIMARY KEY column lists,
-- so we express the (tenant, project-or-tenant-wide, phone) uniqueness as a
-- unique expression index. coalesce() projects null project_id to a sentinel
-- so tenant-wide entries collapse to a single row per phone.
create unique index project_dnc_uq_idx
  on project_dnc (
    tenant_id,
    coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid),
    phone_hash
  );

-- ─── surveys ──────────────────────────────────────────────────────────────────
create table surveys (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  name text not null,
  current_version_id uuid,
  primary_mode text not null default 'form'
    check (primary_mode in ('form','flow')),
  created_at timestamptz not null default now()
);

create table survey_versions (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  survey_id uuid not null references surveys(id) on delete cascade,
  version_label text not null,
  schema jsonb not null,
  is_active boolean not null default false,
  created_at timestamptz not null default now(),
  created_by uuid references users(id)
);
create unique index survey_versions_active_one
  on survey_versions(survey_id) where is_active;

-- now we can satisfy the forward references on projects/surveys
alter table projects
  add constraint projects_survey_id_fk
  foreign key (survey_id) references surveys(id);
alter table projects
  add constraint projects_default_version_fk
  foreign key (default_survey_version_id) references survey_versions(id);
alter table surveys
  add constraint surveys_current_version_fk
  foreign key (current_version_id) references survey_versions(id);

-- ─── dialer / calls ───────────────────────────────────────────────────────────
create table calls (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  project_id uuid not null references projects(id),
  respondent_id uuid references respondents(id),
  operator_id uuid references users(id),
  survey_version_id uuid references survey_versions(id),
  started_at timestamptz not null default now(),
  answered_at timestamptz,
  ended_at timestamptz,
  duration_sec int,
  status text not null default 'in-progress'
    check (status in ('in-progress','success','refused','dropped',
                      'no-answer','busy','callback','wrong-person','tech-failure')),
  hangup_cause text,
  attempt_no int not null default 1,
  trunk_used text,
  sip_call_id text,
  freeswitch_node text,
  comment text
);
create index calls_project_started_idx on calls (project_id, started_at desc);
create index calls_operator_started_idx on calls (operator_id, started_at desc);
create index calls_status_started_idx on calls (status, started_at desc);

create table call_events (
  call_id uuid not null references calls(id) on delete cascade,
  ts timestamptz not null,
  event text not null,
  payload jsonb,
  primary key (call_id, ts, event)
);

create table call_recordings (
  call_id uuid primary key references calls(id) on delete cascade,
  tenant_id uuid not null,
  s3_bucket text not null,
  s3_key text not null,
  duration_sec int not null,
  sha256 text not null,
  codec text not null default 'opus-32',
  encrypted_dek bytea not null,
  kms_key_id text not null,
  retention_until date not null,
  delete_at date,
  created_at timestamptz not null default now()
);
create index call_recordings_retention_idx
  on call_recordings (retention_until)
  where delete_at is null;

create table call_answers (
  call_id uuid not null references calls(id) on delete cascade,
  question_id text not null,
  answer jsonb not null,
  answered_at timestamptz not null default now(),
  primary key (call_id, question_id)
);

-- ─── operator sessions / state log ────────────────────────────────────────────
create table operator_sessions (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  user_id uuid not null references users(id),
  project_id uuid not null references projects(id),
  started_at timestamptz not null default now(),
  ended_at timestamptz,
  total_call_sec int not null default 0,
  total_pause_sec int not null default 0
);
create index operator_sessions_user_idx
  on operator_sessions (user_id, started_at desc);

create table operator_state_log (
  session_id uuid not null references operator_sessions(id) on delete cascade,
  ts timestamptz not null,
  state text not null,
  reason text,
  primary key (session_id, ts)
);

-- ─── audit ────────────────────────────────────────────────────────────────────
create table audit_log (
  id bigserial primary key,
  tenant_id uuid not null,
  actor_kind text not null check (actor_kind in ('user','system','service')),
  actor_user_id uuid,
  action text not null,
  target_kind text not null,
  target_id text,
  payload jsonb,
  ts timestamptz not null default now(),
  ip text,
  user_agent text
);
create index audit_log_tenant_ts_idx on audit_log (tenant_id, ts desc);
create index audit_log_action_ts_idx on audit_log (action, ts desc);

-- ─── reports / async jobs ─────────────────────────────────────────────────────
create table reports_jobs (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null,
  requested_by uuid references users(id),
  kind text not null,
  params jsonb not null,
  status text not null check (status in ('queued','running','succeeded','failed')),
  result_s3_key text,
  error text,
  created_at timestamptz not null default now(),
  finished_at timestamptz
);
create index reports_jobs_queue_idx
  on reports_jobs (status, created_at)
  where status in ('queued','running');

-- ─── RLS policies ─────────────────────────────────────────────────────────────
-- All business tables that carry tenant_id get a tenant-isolation policy.
-- The policy reads `current_setting('app.tenant_id', true)::uuid`. The
-- second argument `true` makes the call return null instead of error if the
-- setting is missing — which combined with `using (tenant_id = null)` returns
-- zero rows, the safe default. Application MUST always set app.tenant_id.

do $$
declare
  t text;
begin
  for t in
    select unnest(array[
      'tenants','tenant_settings','users','user_sessions',
      'projects','project_quotas','project_assignments','respondents','project_dnc',
      'surveys','survey_versions',
      'calls','call_events','call_recordings','call_answers',
      'operator_sessions','operator_state_log',
      'audit_log','reports_jobs'
    ])
  loop
    execute format('alter table %I enable row level security', t);
    execute format('alter table %I force row level security', t);
  end loop;
end$$;

-- Tables whose rows have a direct tenant_id column.
create policy tenants_iso on tenants
  using (id = current_setting('app.tenant_id', true)::uuid)
  with check (id = current_setting('app.tenant_id', true)::uuid);

create policy tenant_settings_iso on tenant_settings
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy users_iso on users
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy user_sessions_iso on user_sessions
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy projects_iso on projects
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- project_quotas / project_assignments don't have tenant_id directly, but
-- their parent project does. Use a sub-query against projects.
create policy project_quotas_iso on project_quotas
  using (project_id in (select id from projects))
  with check (project_id in (select id from projects));

create policy project_assignments_iso on project_assignments
  using (project_id in (select id from projects))
  with check (project_id in (select id from projects));

create policy respondents_iso on respondents
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy project_dnc_iso on project_dnc
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy surveys_iso on surveys
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy survey_versions_iso on survey_versions
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy calls_iso on calls
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- call_events does not carry tenant_id; cascade through calls.
create policy call_events_iso on call_events
  using (call_id in (select id from calls))
  with check (call_id in (select id from calls));

create policy call_recordings_iso on call_recordings
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy call_answers_iso on call_answers
  using (call_id in (select id from calls))
  with check (call_id in (select id from calls));

create policy operator_sessions_iso on operator_sessions
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy operator_state_log_iso on operator_state_log
  using (session_id in (select id from operator_sessions))
  with check (session_id in (select id from operator_sessions));

create policy audit_log_iso on audit_log
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

create policy reports_jobs_iso on reports_jobs
  using (tenant_id = current_setting('app.tenant_id', true)::uuid)
  with check (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ─── grants ───────────────────────────────────────────────────────────────────
-- Application user gets standard CRUD on every table; tenancy_admin already
-- has BYPASSRLS at the role level. We do not yet have a dedicated read-only
-- analytics user — that arrives with Plan 13 (analytics).
do $$
declare
  app_user text;
begin
  app_user := current_user;
  execute format('grant select, insert, update, delete on all tables in schema public to %I', app_user);
  execute format('grant usage, select on all sequences in schema public to %I', app_user);
end$$;

-- tenancy_admin needs DML on the tables the tenancy module owns. BYPASSRLS
-- at the role level lets `set local role tenancy_admin` skip RLS predicates,
-- but the role still has to be granted base table privileges.
grant select, insert, update, delete on tenants to tenancy_admin;
grant select, insert, update, delete on tenant_settings to tenancy_admin;

commit;

-- dev/postgres/init.sql — bootstrap roles + extensions for the local dev DB.
--
-- This script runs ONCE on first start of the postgres container (when the
-- data volume is empty). It mirrors the production role layout (see Plan 03)
-- so that RLS behaves identically between dev and prod.
--
-- DEV ONLY. Production roles + passwords are managed by Yandex MPG.

-- The application role (used by cmd/api connection pool, RLS-enforced).
-- Already exists from POSTGRES_USER=app env var; ensure password is what we
-- expect for connection strings checked into configs/development/config.yaml.
ALTER ROLE app WITH LOGIN PASSWORD 'devpass';

-- The tenancy admin role (BYPASSRLS, used only by the tenancy module to
-- provision new tenants). Mirrors the prod layout introduced in Plan 03.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tenancy_admin') THEN
        CREATE ROLE tenancy_admin WITH LOGIN BYPASSRLS PASSWORD 'dev_tenancy_password';
    END IF;
END
$$;

GRANT ALL PRIVILEGES ON DATABASE sociopulse TO tenancy_admin;

-- Ensure tenancy_admin owns the public schema (so it can issue DDL),
-- and app has USAGE rights to read/write tenant-scoped tables once they exist.
ALTER SCHEMA public OWNER TO tenancy_admin;
GRANT USAGE ON SCHEMA public TO app;

-- Required extensions (subset of production, see Plan 03).
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";

// Package postgres is the СоциоПульс application-side database layer.
//
// It wraps github.com/jackc/pgx/v5/pgxpool with the conventions required by
// the spec:
//
//   - Every business query runs inside a transaction.
//   - Every transaction starts with `SET LOCAL app.tenant_id = '<uuid>'`,
//     which the RLS policies in migrations/000001_init.up.sql rely on.
//   - The tenancy module's cross-tenant operations run under a separate
//     code path (BypassRLS) that uses `SET LOCAL ROLE tenancy_admin`.
//
// The package is intentionally thin: most of the SQL is written elsewhere
// (in module-specific store packages). This package owns connection
// lifecycle, tenant isolation, and a small set of typed errors.
//
// The depguard rule pgxpool-isolation (Plan 00a Task 8) blocks direct
// imports of jackc/pgx/v5/pgxpool from anywhere outside this package and
// the explicitly allowed admin paths, so application code is forced
// through Pool.WithTenant or Pool.BypassRLS.
//
// See ADR-0006 (RLS + SET LOCAL) and Plan 03 Task 4.
package postgres

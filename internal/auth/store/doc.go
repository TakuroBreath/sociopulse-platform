// Package store contains the Postgres-backed implementations of the
// internal/auth/api persistence ports.
//
// Mutating methods take a postgres.Tx so the caller (the auth service
// layer) can co-locate row writes with audit / outbox writes inside a
// single transaction, mirroring the pattern established by
// internal/tenancy/store. Read methods accept the same Tx — the auth
// service typically opens a per-tenant transaction (pool.WithTenant)
// and chains every CRUD call through it so RLS isolation kicks in.
//
// Package boundary: cross-module callers MUST import from
// internal/auth/api only — depguard's module-boundaries rule rejects
// direct imports of this package from outside the auth module.
package store

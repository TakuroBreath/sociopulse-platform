// Package store is the persistence layer of the tenancy module. The
// concrete PostgresStore implements api.Store and connects through the
// `tenancy_admin` Postgres role (BYPASSRLS) via *postgres.Pool.BypassRLS.
//
// The store is the only path in the codebase that may safely read or write
// across tenants; depguard prevents other modules from importing this
// package directly. All higher-level traffic flows through
// internal/tenancy/api.Tenancy and the service composition root.
package store

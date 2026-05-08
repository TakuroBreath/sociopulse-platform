// Package store provides Postgres-backed implementations of the crm api
// persistence ports. The package is private to the crm module — cross-
// module callers go through internal/crm/api, the depguard
// module-boundaries rule rejects any direct import.
//
// Mutating methods accept a postgres.Tx so the crm service layer can co-
// locate the row write with audit and outbox writes in the same
// transaction. Read methods take the same Tx — the service is expected
// to open a per-tenant transaction (Pool.WithTenant) and chain every
// store call through it so the RLS policy applies uniformly.
package store

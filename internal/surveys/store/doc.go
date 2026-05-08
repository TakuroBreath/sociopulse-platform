// Package store implements the Postgres-backed adapters for the surveys
// module. The two adapters — SurveyStore and VersionStore — satisfy
// internal/surveys/api.SurveyStorePort and api.VersionStorePort
// respectively. They are the only place in the surveys module that
// imports pgx; service-layer code (internal/surveys/service) consumes
// the api ports and never sees pgx types.
//
// Cross-module imports are blocked by depguard's module-boundaries
// rule — only internal/surveys/api is reachable from outside the
// surveys module. Within the surveys module, internal/surveys/module.go
// composes these stores with the service so the production wiring lives
// at the boundary.
//
// Transactions: every mutating method takes a postgres.Tx so the
// caller (the service layer) can co-locate row writes with audit and
// outbox writes in the same commit. Read methods take the same Tx so
// RLS scoping (set-via-WithTenant) covers them too. The store never
// owns the transaction lifecycle.
package store

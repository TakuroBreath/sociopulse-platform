// Package service is the business-logic layer of the tenancy module. It
// composes a persistence Store (typically internal/tenancy/store.PostgresStore
// connected through the tenancy_admin BYPASSRLS Postgres role), a KMS client
// for per-tenant KEK provisioning, and a publisher for lifecycle / cache-
// invalidation events.
//
// Direct imports of this package are forbidden by depguard; downstream code
// reaches the surface through internal/tenancy/api.Tenancy. The composition
// root in internal/tenancy/module.go wires the concrete implementation by
// installing service.Register into api.Register.
package service
